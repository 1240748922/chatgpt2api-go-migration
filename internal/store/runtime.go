package store

import (
	"encoding/json"
)

func (s *Store) PublishRuntime(id string, snapshot map[string]any) error {
	if s.pg == nil {
		return nil
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	ctx, cancel := dbContext()
	defer cancel()
	_, err = s.pg.ExecContext(ctx, `INSERT INTO go_runtime_instances VALUES($1,$2,now()+interval '10 seconds') ON CONFLICT(id) DO UPDATE SET data=excluded.data,expires_at=excluded.expires_at`, id, raw)
	return err
}
func (s *Store) RuntimeSnapshots() ([]map[string]any, error) {
	out := []map[string]any{}
	if s.pg == nil {
		return out, nil
	}
	ctx, cancel := dbContext()
	defer cancel()
	rows, err := s.pg.QueryContext(ctx, `SELECT id,data FROM go_runtime_instances WHERE expires_at>now()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var doc map[string]any
		if err = json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
		doc["instance_id"] = id
		out = append(out, doc)
	}
	return out, rows.Err()
}
func (s *Store) RemoveRuntime(id string) error {
	if s.pg == nil {
		return nil
	}
	ctx, cancel := dbContext()
	defer cancel()
	_, err := s.pg.ExecContext(ctx, `DELETE FROM go_runtime_instances WHERE id=$1 OR expires_at<now()-interval '1 day'`, id)
	return err
}
func (s *Store) RequestRuntimeCancel(id, instance string) error {
	ctx, cancel := dbContext()
	defer cancel()
	_, err := s.pg.ExecContext(ctx, `INSERT INTO go_runtime_cancels VALUES($1,$2,now()+interval '30 seconds') ON CONFLICT(call_id) DO NOTHING`, id, instance)
	return err
}
func (s *Store) ConsumeRuntimeCancels(instance string) ([]string, error) {
	ctx, cancel := dbContext()
	defer cancel()
	rows, err := s.pg.QueryContext(ctx, `DELETE FROM go_runtime_cancels WHERE instance_id=$1 OR expires_at<now() RETURNING call_id,(expires_at>now() AND instance_id=$1)`, instance)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		var valid bool
		if err = rows.Scan(&id, &valid); err != nil {
			return nil, err
		}
		if valid {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}
