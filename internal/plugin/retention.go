package plugin

import "time"

// startPrune runs the retention job now and then every pruneEvery, until
// Close. On a reconfigure the loop is already running, so it only prunes now
// with the new retention.
func (p *Plugin) startPrune() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruning.Add(1)
	if p.pruneStop != nil {
		go func() {
			defer p.pruning.Done()
			p.pruneOnce()
		}()
		return
	}
	stop := make(chan struct{})
	p.pruneStop = stop
	go func() {
		defer p.pruning.Done()
		p.pruneOnce()
		ticker := time.NewTicker(pruneEvery)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				p.pruneOnce()
			}
		}
	}()
}

// pruneOnce deletes rows older than retention_days. Zero keeps everything.
func (p *Plugin) pruneOnce() {
	p.mu.Lock()
	s, days := p.store, p.cfg.RetentionDays
	p.mu.Unlock()
	if s == nil || days <= 0 {
		return
	}
	n, err := s.Prune(time.Now().AddDate(0, 0, -days))
	if err != nil {
		p.logf("warn", "cpa-analytics: retention prune failed: %v", err)
		return
	}
	if n > 0 {
		p.logf("info", "cpa-analytics: pruned %d rows older than %d days", n, days)
	}
}
