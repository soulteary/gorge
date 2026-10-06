package taskqueue

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/soulteary/gorge/go/internal/contracts"
)

var finalizeExecutionScript = redis.NewScript(`
local prefix, id, owner, expiry, duration, now = ARGV[1], ARGV[2], ARGV[3], tonumber(ARGV[4]), ARGV[5], tonumber(ARGV[6])
local task, archived = prefix..'task:'..id, prefix..'archived:'..id
local function owns(key)
 return redis.call('HGET', key, 'leaseOwner') == owner and tonumber(redis.call('HGET', key, 'leaseExpires')) == expiry
end
if redis.call('EXISTS', task) == 0 then
 if owns(archived) and redis.call('HGET', archived, 'result') == '0' then return 1 end
 return redis.error_reply('LEASE_CONFLICT')
end
if not owns(task) or expiry <= now then return redis.error_reply('LEASE_CONFLICT') end
local children = cjson.decode(ARGV[7])
local priority = tonumber(redis.call('HGET', task, 'priority'))
for _, child in ipairs(children) do
 local childID = redis.call('INCR', prefix..'next_task_id')
 local dataID = redis.call('INCR', prefix..'next_data_id')
 local p = priority
 if child.priority ~= nil and child.priority ~= cjson.null then p = child.priority end
 local delay = child.delayUntil
 local ck = prefix..'task:'..childID
 redis.call('SET', prefix..'data:'..dataID, child.data)
 redis.call('HSET', ck, 'id', childID, 'taskClass', child.taskClass, 'dataID', dataID,
  'priority', p, 'failureCount', 0, 'dateCreated', now, 'dateModified', now, 'leaseOwner', '')
 if child.objectPHID then redis.call('HSET', ck, 'objectPHID', child.objectPHID) end
 if child.containerPHID then redis.call('HSET', ck, 'containerPHID', child.containerPHID) end
 local score = p*1e12+childID
 redis.call('ZADD', prefix..'idx:active', score, childID)
 if delay ~= nil and delay ~= cjson.null and delay > 0 then
  redis.call('HSET', ck, 'leaseExpires', delay)
  redis.call('ZADD', prefix..'idx:leased', delay, childID)
 else redis.call('ZADD', prefix..'idx:unleased', score, childID) end
end
local fields = redis.call('HGETALL', task)
for i=1,#fields,2 do redis.call('HSET', archived, fields[i], fields[i+1]) end
redis.call('HSET', archived, 'result', 0, 'duration', duration, 'archivedEpoch', now, 'dateModified', now)
local dataID = redis.call('HGET', task, 'dataID')
local data = redis.call('GET', prefix..'data:'..dataID)
if data then redis.call('HSET', archived, 'data', data) end
redis.call('ZREM', prefix..'idx:active', id)
redis.call('ZREM', prefix..'idx:unleased', id)
redis.call('ZREM', prefix..'idx:leased', id)
redis.call('ZADD', prefix..'idx:archived', now, id)
redis.call('INCR', prefix..'counter:archived')
if tonumber(redis.call('HGET', task, 'failureCount') or '0') > 0 then redis.call('DECR', prefix..'counter:failed') end
redis.call('DEL', task)
return 1
`)

func (s *RedisStore) Finalize(ctx context.Context, req *contracts.FinalizeRequest) error {
	children := req.Followups
	if children == nil {
		children = []contracts.EnqueueRequest{}
	}
	raw, err := json.Marshal(children)
	if err != nil {
		return err
	}
	err = finalizeExecutionScript.Run(ctx, s.rdb, []string{s.taskKey(req.TaskID)},
		s.prefix, req.TaskID, req.LeaseOwner, req.LeaseExpires, req.Duration, time.Now().Unix(), string(raw)).Err()
	return redisExecutionError(err)
}

var renewExecutionScript = redis.NewScript(`
local key, owner, expiry, now, duration = KEYS[1], ARGV[1], tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4])
if redis.call('HGET', key, 'leaseOwner') ~= owner or tonumber(redis.call('HGET', key, 'leaseExpires')) ~= expiry or expiry <= now then
 return redis.error_reply('LEASE_CONFLICT')
end
local newExpiry = math.max(expiry, now+duration)
redis.call('HSET', key, 'leaseExpires', newExpiry)
redis.call('ZADD', KEYS[2], newExpiry, ARGV[5])
return newExpiry
`)

func (s *RedisStore) Renew(ctx context.Context, req *contracts.RenewRequest) (*contracts.Task, error) {
	expiry, err := renewExecutionScript.Run(ctx, s.rdb, []string{s.taskKey(req.TaskID), s.leasedSetKey()},
		req.LeaseOwner, req.LeaseExpires, time.Now().Unix(), req.Duration, req.TaskID).Int64()
	if err != nil {
		return nil, redisExecutionError(err)
	}
	task, err := s.loadTask(ctx, req.TaskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, ErrLeaseConflict
	}
	task.LeaseExpires = &expiry
	return task, nil
}
func redisExecutionError(err error) error {
	if err != nil && strings.Contains(err.Error(), "LEASE_CONFLICT") {
		return ErrLeaseConflict
	}
	return err
}
