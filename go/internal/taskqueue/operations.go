package taskqueue

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/soulteary/gorge/go/internal/platform/operations"
	"strings"
	"time"
)

func (s *MySQLStore) Operations(ctx context.Context) (map[string]any, error) {
	return operations.MySQL(ctx, s.db, []operations.Table{{Name: "worker_activetask"}, {Name: "worker_archivetask"}, {Name: "worker_gorgeinbox"}, {Name: "worker_gorgeschedulercontrol"}, {Name: "worker_taskdata"}, {Name: "lisk_counter"}}), nil
}
func (s *RedisStore) Operations(ctx context.Context) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stats, err := s.Stats(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"backend": "redis", "stats": stats, "configuredBackendIdentity": fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%s", s.rdb.Options().Addr, s.rdb.Options().DB, s.prefix)))), "physicalIdentityState": "not_proven", "snapshot": "observational"}
	var cursor uint64
	var examined, memory, inbox int64
	memoryIncomplete := false
	seen := map[string]bool{}
	for {
		keys, next, e := s.rdb.Scan(ctx, cursor, s.prefix+"*", 256).Result()
		if e != nil {
			return nil, e
		}
		for _, key := range keys {
			if !strings.HasPrefix(key, s.prefix) || seen[key] {
				continue
			}
			seen[key] = true
			examined++
			if strings.HasPrefix(key, s.prefix+"inbox:") {
				inbox++
			}
			n, e := s.rdb.MemoryUsage(ctx, key).Result()
			if e == nil {
				memory += n
			} else {
				memoryIncomplete = true
			}
		}
		cursor = next
		if cursor == 0 || examined >= 10000 {
			break
		}
	}
	out["examinedKeys"] = examined
	out["approximateMemoryBytes"] = memory
	out["inboxRecords"] = inbox
	out["truncated"] = cursor != 0
	out["memoryIncomplete"] = memoryIncomplete
	archive, err := s.rdb.ZCard(ctx, s.archivedIndexKey()).Result()
	if err != nil {
		return nil, err
	}
	out["archivedIndexRecords"] = archive
	out["finalizeDeduplication"] = "archived task identity and lease fencing; retain archive"
	return out, nil
}
