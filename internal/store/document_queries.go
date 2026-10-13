package store

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"
)

// Filter and page in PostgreSQL rather than loading every historical job.
func (s *Store) ListDocuments(prefix, owner string, statuses []string, limit int) ([]map[string]any, error) {
	return s.listDocuments(prefix, owner, statuses, limit, false, false)
}

func (s *Store) PendingDocuments(prefix string, statuses []string, limit int) ([]map[string]any, error) {
	return s.listDocuments(prefix, "", statuses, limit, true, false)
}

func (s *Store) listDocuments(prefix, owner string, statuses []string, limit int, oldest, due bool) ([]map[string]any, error) {
	out := []map[string]any{}
	limit = max(1, min(limit, 200))
	if s.pg == nil {
		names, err := s.DocumentNames(prefix)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			doc, e := s.Document(name)
			if e != nil {
				return nil, e
			}
			if owner != "" && stringValue(doc["owner_id"]) != owner {
				continue
			}
			if len(statuses) > 0 && !hasDocumentStatus(statuses, stringValue(doc["status"])) {
				continue
			}
			if due {
				next, _ := time.Parse(time.RFC3339Nano, stringValue(doc["next_retry_at"]))
				if time.Now().Before(next) {
					continue
				}
			}
			doc["document_name"] = name
			out = append(out, doc)
		}
		sort.Slice(out, func(i, j int) bool {
			a, b := stringValue(out[i]["updated_at"]), stringValue(out[j]["updated_at"])
			if a == b {
				return stringValue(out[i]["document_name"]) < stringValue(out[j]["document_name"])
			}
			if oldest {
				return a < b
			}
			return a > b
		})
		return out[:min(limit, len(out))], nil
	}
	ctx, cancel := dbContext()
	defer cancel()
	order := "DESC"
	if oldest {
		order = "ASC"
	}
	rows, err := s.pg.QueryContext(ctx, `SELECT name,data FROM go_documents WHERE starts_with(name,$1) AND ($2='' OR data->>'owner_id'=$2) AND (coalesce(cardinality($3::text[]),0)=0 OR data->>'status'=ANY($3)) AND ($5=false OR CASE WHEN pg_input_is_valid(data->>'next_retry_at','timestamp with time zone') THEN (data->>'next_retry_at')::timestamptz ELSE '-infinity'::timestamptz END<=$6::timestamptz) ORDER BY updated_at `+order+`,name ASC LIMIT $4`, prefix, owner, statuses, limit, due, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var raw []byte
		if err = rows.Scan(&name, &raw); err != nil {
			return nil, err
		}
		var doc map[string]any
		if err = json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
		doc["document_name"] = name
		out = append(out, doc)
	}
	return out, rows.Err()
}
func hasDocumentStatus(statuses []string, status string) bool {
	for _, s := range statuses {
		if s == status {
			return true
		}
	}
	return false
}

// AllDocuments reads the gallery index in one scan instead of one query per image.
func (s *Store) AllDocuments(prefix string, statuses []string) ([]map[string]any, error) {
	if s.pg == nil {
		names, err := s.DocumentNames(prefix)
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, name := range names {
			d, e := s.Document(name)
			if e != nil {
				return nil, e
			}
			if len(statuses) == 0 || hasDocumentStatus(statuses, stringValue(d["status"])) {
				d["document_name"] = name
				out = append(out, d)
			}
		}
		return out, nil
	}
	ctx, cancel := dbContext()
	defer cancel()
	rows, err := s.pg.QueryContext(ctx, `SELECT name,data FROM go_documents WHERE starts_with(name,$1) AND (coalesce(cardinality($2::text[]),0)=0 OR data->>'status'=ANY($2)) ORDER BY name`, prefix, statuses)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name string
		var raw []byte
		if err = rows.Scan(&name, &raw); err != nil {
			return nil, err
		}
		var d map[string]any
		if err = json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		d["document_name"] = name
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *Store) DeleteDocument(name string) error {
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return os.ErrInvalid
	}
	if s.pg != nil {
		ctx, cancel := dbContext()
		defer cancel()
		_, err := s.pg.ExecContext(ctx, `DELETE FROM go_documents WHERE name=$1`, name)
		return err
	}
	s.documentMu.Lock()
	defer s.documentMu.Unlock()
	err := os.Remove(s.documentPath(name))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *Store) PendingDueDocuments(prefix string, statuses []string, limit int) ([]map[string]any, error) {
	return s.listDocuments(prefix, "", statuses, limit, true, true)
}
