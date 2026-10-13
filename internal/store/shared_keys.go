package store

import (
	"crypto/subtle"
	"errors"
	"os"
	"strings"
	"time"
)

func docItems(doc map[string]any) []map[string]any {
	out := []map[string]any{}
	switch v := doc["items"].(type) {
	case []map[string]any:
		return v
	case []any:
		for _, x := range v {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}
func (s *Store) pgAuthenticate(token string) (Identity, bool) {
	doc, err := s.Document("auth-keys")
	if err != nil {
		return Identity{}, false
	}
	candidate := hashKey(token)
	for _, item := range docItems(doc) {
		if boolValue(item["enabled"], true) && subtle.ConstantTimeCompare([]byte(candidate), []byte(stringValue(item["key_hash"]))) == 1 {
			return identityFromItem(item), true
		}
	}
	return Identity{}, false
}
func (s *Store) pgCreateKey(role, name, adminKey string) (map[string]any, string, error) {
	role = strings.ToLower(strings.TrimSpace(role))
	if role != "admin" && role != "user" {
		return nil, "", errors.New("invalid key role")
	}
	raw, err := randomKey()
	if err != nil {
		return nil, "", err
	}
	var item map[string]any
	_, err = s.MutateDocument("auth-keys", func(doc map[string]any) error {
		items := docItems(doc)
		item = map[string]any{"id": randomID(), "name": uniqueName(items, name, role), "role": role, "key_hash": hashKey(raw), "enabled": true, "created_at": time.Now().UTC().Format(time.RFC3339)}
		doc["items"] = append(items, item)
		return nil
	})
	return publicKey(item), raw, err
}
func (s *Store) pgUpdateKey(id, role string, updates map[string]any, adminKey string) (map[string]any, error) {
	var result map[string]any
	_, err := s.MutateDocument("auth-keys", func(doc map[string]any) error {
		items := docItems(doc)
		for i, item := range items {
			if stringValue(item["id"]) != id {
				continue
			}
			if role != "" && !strings.EqualFold(role, stringValue(item["role"])) {
				return os.ErrNotExist
			}
			next := cloneMap(item)
			if name, ok := updates["name"]; ok {
				next["name"] = uniqueNameExcluding(items, stringValue(name), stringValue(item["role"]), id)
			}
			if v, ok := updates["enabled"]; ok {
				next["enabled"] = boolValue(v, true)
			}
			if v, ok := updates["key"]; ok {
				key := stringValue(v)
				if key == "" || key == adminKey {
					return errors.New("invalid key")
				}
				next["key_hash"] = hashKey(key)
			}
			items[i] = next
			doc["items"] = items
			result = publicKey(next)
			return nil
		}
		return os.ErrNotExist
	})
	return result, err
}
func (s *Store) pgDeleteKey(id, role string) error {
	_, err := s.MutateDocument("auth-keys", func(doc map[string]any) error {
		items := docItems(doc)
		out := []map[string]any{}
		found := false
		for _, item := range items {
			if stringValue(item["id"]) == id && (role == "" || strings.EqualFold(role, stringValue(item["role"]))) {
				found = true
				continue
			}
			out = append(out, item)
		}
		if !found {
			return os.ErrNotExist
		}
		doc["items"] = out
		return nil
	})
	return err
}
