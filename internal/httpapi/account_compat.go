package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

type accountSelection struct {
	Mode     string   `json:"mode"`
	IDs      []string `json:"account_ids"`
	Excluded []string `json:"excluded_account_ids"`
	Keyword  string   `json:"keyword"`
	Status   string   `json:"status"`
	Group    string   `json:"group_id"`
}

// Return stable IDs, never credentials, and resolve a large selection in one scan.
func selectAccountIDs(items []map[string]any, selection accountSelection) (ids, missing, excluded []string, matching int, err error) {
	if selection.Mode == "" {
		selection.Mode = "explicit"
	}
	if selection.Mode != "explicit" && selection.Mode != "filtered" {
		return nil, nil, nil, 0, fmt.Errorf("invalid selection mode")
	}
	wanted, omit, found := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, id := range selection.IDs {
		wanted[strings.TrimSpace(id)] = true
	}
	for _, id := range selection.Excluded {
		omit[strings.TrimSpace(id)] = true
	}
	keyword := strings.ToLower(strings.TrimSpace(selection.Keyword))
	for _, item := range items {
		id := accountPublicRef(item)
		if selection.Mode == "explicit" {
			if !wanted[id] {
				continue
			}
			found[id] = true
		} else if !accountStatusMatches(item, selection.Status) || !accountGroupMatches(item, selection.Group) || (keyword != "" && !accountContains(item, keyword)) {
			continue
		}
		matching++
		if selection.Mode == "filtered" && omit[id] {
			excluded = append(excluded, id)
			continue
		}
		ids = append(ids, id)
	}
	if selection.Mode == "explicit" {
		for _, id := range uniqueAccountRefs(selection.IDs) {
			if !found[id] {
				missing = append(missing, id)
			}
		}
	}
	return
}

func (s *Server) accountSelectionPreview(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		Selection accountSelection `json:"selection"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	items, _, err := s.store.AccountSnapshot()
	if err != nil {
		writeError(w, 503, "读取账号失败", "storage_error")
		return
	}
	ids, missing, excluded, matching, err := selectAccountIDs(items, body.Selection)
	if err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	errorsOut := []map[string]any{}
	for _, id := range missing {
		errorsOut = append(errorsOut, map[string]any{"account_id": id, "error": "account not found"})
	}
	if excluded == nil {
		excluded = []string{}
	}
	writeJSON(w, 200, map[string]any{"matching_count": matching, "selected_count": len(ids), "excluded_account_ids": excluded, "errors": errorsOut})
}

func (s *Server) accountDetailCompat(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/accounts/"), "/")
	if len(parts) == 2 && parts[1] == "test" && r.Method == http.MethodPost {
		s.testSelectedAccount(w, r, parts[0])
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	if len(parts) > 2 || parts[0] == "" {
		writeError(w, 404, "account not found", "not_found")
		return
	}
	item, err := s.accountByRef(parts[0])
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, 404, "account not found", "not_found")
		} else {
			writeError(w, 503, "读取账号失败", "storage_error")
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if len(parts) == 1 {
		writeJSON(w, 200, map[string]any{"item": accountForAPI(item, s.configuredImageMinimumValidity())})
		return
	}
	key := map[string]string{"access-token": "access_token", "refresh-token": "refresh_token"}[parts[1]]
	if key == "" || stringValue(item[key]) == "" {
		writeError(w, 404, "credential not found", "not_found")
		return
	}
	writeJSON(w, 200, map[string]any{key: item[key]})
}

func (s *Server) selectedOperationRefs(tokens, ids []string, selection *accountSelection) ([]string, error) {
	if selection == nil {
		return uniqueAccountRefs(append(tokens, ids...)), nil
	}
	items, _, err := s.store.AccountSnapshot()
	if err != nil {
		return nil, err
	}
	refs, missing, _, _, err := selectAccountIDs(items, *selection)
	return append(refs, missing...), err
}
