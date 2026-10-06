package taskqueue

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/soulteary/gorge/go/internal/contracts"
)

var enqueueEventScript = redis.NewScript(`
local prefix,event,digest,now=ARGV[1],ARGV[2],ARGV[3],tonumber(ARGV[4])
local receipt=KEYS[1]
local stored=redis.call('HGET',receipt,'hash')
if stored then
 if stored~=digest then return redis.error_reply('EVENT_CONFLICT') end
 return redis.call('HGET',receipt,'response')
end
local input=cjson.decode(ARGV[5])
local id=redis.call('INCR',prefix..'next_task_id')
local dataID=redis.call('INCR',prefix..'next_data_id')
local priority=input.priority or 2000
if priority==cjson.null then priority=2000 end
local key=prefix..'task:'..id
redis.call('SET',prefix..'data:'..dataID,input.data)
redis.call('HSET',key,'id',id,'dataID',dataID,'taskClass',input.taskClass,'priority',priority,'failureCount',0,'dateCreated',now,'dateModified',now,'leaseOwner','')
local response={id=id,dataID=dataID,taskClass=input.taskClass,data=input.data,priority=priority,failureCount=0,dateCreated=now,dateModified=now}
if input.objectPHID then redis.call('HSET',key,'objectPHID',input.objectPHID);response.objectPHID=input.objectPHID end
if input.containerPHID then redis.call('HSET',key,'containerPHID',input.containerPHID);response.containerPHID=input.containerPHID end
local score=priority*1e12+id
redis.call('ZADD',prefix..'idx:active',score,id)
local delay=input.delayUntil
if delay and delay~=cjson.null and delay>0 then
 redis.call('HSET',key,'leaseExpires',delay);redis.call('ZADD',prefix..'idx:leased',delay,id);response.leaseExpires=delay
else redis.call('ZADD',prefix..'idx:unleased',score,id) end
local raw=cjson.encode(response)
redis.call('HSET',receipt,'hash',digest,'response',raw)
return raw
`)

func (s *RedisStore) EnqueueEvent(ctx context.Context, req *contracts.EnqueueEventRequest) (*contracts.Task, error) {
	digest, raw, err := eventDigest(req)
	if err != nil {
		return nil, err
	}
	response, err := enqueueEventScript.Run(ctx, s.rdb, []string{s.prefix + "inbox:" + req.EventID}, s.prefix, req.EventID, digest, time.Now().Unix(), string(raw)).Text()
	if err != nil {
		if strings.Contains(err.Error(), "EVENT_CONFLICT") {
			return nil, ErrEventConflict
		}
		return nil, err
	}
	task := new(contracts.Task)
	err = json.Unmarshal([]byte(response), task)
	return task, err
}
