package store

import "os"

// Match an RT duplicate without overwriting a newer rotated credential set.
func (s *Store) FindCredential(input map[string]any) (map[string]any, error) {
	if id := stringValue(input["account_id"]); id != "" {
		return s.FindAccount(id)
	}
	if rt := stringValue(input["refresh_token"]); rt != "" && s.pg != nil {
		ctx, cancel := dbContext()
		defer cancel()
		var id string
		if err := s.pg.QueryRowContext(ctx, `SELECT id FROM go_accounts WHERE rt_key=$1 AND NOT deleted`, hashKey(rt)).Scan(&id); err == nil {
			return s.FindAccount(id)
		}
	}
	if at := accountToken(input); at != "" {
		if item, e := s.FindAccount(at); e == nil {
			return item, nil
		}
	}
	if s.pg == nil {
		items, _, err := s.AccountSnapshot()
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if rt := stringValue(input["refresh_token"]); rt != "" && rt == stringValue(item["refresh_token"]) {
				return cloneMap(item), nil
			}
		}
	}
	return nil, os.ErrNotExist
}
