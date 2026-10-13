package httpapi

import (
	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"time"
)

func (s *Server) pagePrewarmScheduler() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	offset := 0
	for {
		select {
		case <-s.backgroundCtx.Done():
			return
		case <-ticker.C:
		}
		cfg, err := s.store.Config()
		if err != nil || !boolValue(mapValue(cfg["page_prewarm"])["enabled"], true) {
			continue
		}
		if intValue(s.store.LatencyMetrics()["account_write_p95_ms"]) > 1000 {
			continue
		}
		items, _, err := s.store.AccountSnapshot()
		if err != nil || len(items) == 0 {
			continue
		}
		minimum := s.configuredImageMinimumValidity()
		ready := 0
		n := 0
		for ; n < len(items) && ready < 2; n++ {
			item := items[(offset+n)%len(items)]
			a := accounts.Account{Token: accountToken(item), Fields: item}
			if !imageModelAccount(a, "gpt-image-2") || !accounts.ImageEligible(item, false, time.Now(), minimum) {
				continue
			}
			_ = s.openAIImage.Prewarm(s.backgroundCtx, a)
			ready++
		}
		offset = (offset + n) % len(items)
	}
}
