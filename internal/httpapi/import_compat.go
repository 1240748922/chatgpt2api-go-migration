package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/ingest"
	"github.com/auucoder/gptgrok2api-go/internal/store"
)

// Legacy endpoints still return committed insert counts synchronously. OpenAI
// credentials use the same durable checkpoints and background sync as the UI.
// Opaque Grok SSO imports retain their existing semantics.
func (s *Server) addImportedAccounts(parent context.Context, tokens []string, payloads []map[string]any) (added, skipped int, items []map[string]any, jobID string, err error) {
	var openAI, grok []map[string]any
	var grokTokens []string
	for _, token := range tokens {
		if strings.HasPrefix(strings.TrimSpace(token), "rt.") {
			openAI = append(openAI, map[string]any{"refresh_token": token})
		} else if isOpenAIAccount(accounts.Account{Token: token}) {
			openAI = append(openAI, map[string]any{"access_token": token})
		} else {
			grokTokens = append(grokTokens, token)
		}
	}
	for _, payload := range payloads {
		item := normalizeImportedAccount(payload)
		if stringValue(item["refresh_token"]) != "" || isOpenAIAccount(accounts.Account{Token: accountToken(item), Fields: item}) {
			openAI = append(openAI, item)
		} else {
			grok = append(grok, item)
		}
	}
	if len(openAI) > 0 {
		raw, e := json.Marshal(openAI)
		if e != nil {
			err = e
			return
		}
		rows, e := ingest.Parse(string(raw), "auto")
		if e != nil {
			err = e
			return
		}
		// Keep explicitly supplied login credentials, but never imported proxy,
		// quota or cooldown metadata. Those are not current upstream state.
		byCredential := map[string]map[string]any{}
		for _, item := range openAI {
			byCredential[firstNonEmpty(stringValue(item["refresh_token"]), stringValue(item["access_token"]))] = item
		}
		for _, row := range rows {
			item := byCredential[firstNonEmpty(stringValue(row["refresh_token"]), stringValue(row["access_token"]))]
			for _, key := range []string{"login_password", "two_factor_secret"} {
				if value := stringValue(item[key]); value != "" {
					row[key] = value
				}
			}
		}
		jobID = newImportID()
		ctx, cancel := context.WithCancel(parent)
		defer cancel()
		lease, e := s.store.AcquireLease(ctx, "import:"+jobID, 120*time.Second)
		if e != nil {
			err = e
			return
		}
		if lease == nil {
			err = fmt.Errorf("import job is busy")
			return
		}
		defer lease.Release()
		lease.KeepAlive(ctx, 120*time.Second, cancel)
		doc, e := s.createImportJobWithID(rows, false, jobID)
		if e == nil {
			e = s.importJobChunks(ctx, lease, jobID, doc)
		}
		if e != nil {
			if ctx.Err() == nil {
				s.importFailure(lease, jobID, e)
			}
			err = e
			return
		}
		if !s.reconcileImportProgressWithLease(lease, jobID, true) {
			err = fmt.Errorf("save import progress failed")
			return
		}
		doc, e = s.store.Document(importDocument(jobID))
		if e != nil {
			err = e
			return
		}
		added, skipped = intValue(doc["added"]), intValue(doc["skipped"])
	}
	if len(grokTokens) > 0 || len(grok) > 0 {
		a, b, _, e := s.store.AddAccounts(grokTokens, grok)
		if e != nil {
			err = e
			return
		}
		added, skipped = added+a, skipped+b
	}
	items, _, err = s.store.AccountSnapshot()
	return
}

func (s *Server) importJobChunks(ctx context.Context, lease *store.Lease, id string, doc map[string]any) error {
	for i := 0; i < intValue(doc["chunks"]); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk, err := s.store.Document(importChunk(id, i))
		if err != nil {
			return err
		}
		if !boolValue(chunk["imported"], false) {
			if _, _, err = lease.ImportChunk(importChunk(id, i), importRows(chunk["accounts"])); err != nil {
				return err
			}
		}
	}
	return nil
}
