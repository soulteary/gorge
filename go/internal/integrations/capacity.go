package integrations

import (
	"context"
	"errors"
)

type TableCapacity struct {
	Table                 string `json:"table"`
	ApproximateDataBytes  int64  `json:"approximateDataBytes"`
	ApproximateIndexBytes int64  `json:"approximateIndexBytes"`
}

// Capacity returns approximate allocated table sizes, not payloads or row IDs.
// Tombstones and resolution evidence must survive restore and late replays.
func (s Store) Capacity(ctx context.Context) ([]TableCapacity, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT TABLE_NAME,COALESCE(DATA_LENGTH,0),COALESCE(INDEX_LENGTH,0)
 FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE()
 AND TABLE_NAME IN ('gorge_integration_effect','gorge_integration_inbox','gorge_integration_resolution') ORDER BY TABLE_NAME`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []TableCapacity{}
	for rows.Next() {
		var v TableCapacity
		if err = rows.Scan(&v.Table, &v.ApproximateDataBytes, &v.ApproximateIndexBytes); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(out) != 3 {
		return nil, errors.New("integration capacity schema incomplete")
	}
	return out, nil
}
