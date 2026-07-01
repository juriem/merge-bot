package main

import (
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func validConfig() commonConfig {
	return commonConfig{
		interval:         ptr(30 * time.Second),
		timeout:          ptr(time.Hour),
		mergeMethod:      ptr("squash"),
		minApprovals:     ptr(2),
		rateLimitWait:    ptr(15 * time.Minute),
		rateLimitRetries: ptr(3),
	}
}

func Test_commonConfig_validate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*commonConfig)
		wantErr bool
	}{
		{"valid", func(*commonConfig) {}, false},
		{"zero interval", func(c *commonConfig) { c.interval = ptr(time.Duration(0)) }, true},
		{"negative timeout", func(c *commonConfig) { c.timeout = ptr(-time.Second) }, true},
		{"bad merge method", func(c *commonConfig) { c.mergeMethod = ptr("fast-forward") }, true},
		{"rebase is valid", func(c *commonConfig) { c.mergeMethod = ptr("rebase") }, false},
		{"negative approvals", func(c *commonConfig) { c.minApprovals = ptr(-1) }, true},
		{"zero approvals ok", func(c *commonConfig) { c.minApprovals = ptr(0) }, false},
		{"negative rate-limit wait", func(c *commonConfig) { c.rateLimitWait = ptr(-time.Minute) }, true},
		{"zero rate-limit wait ok", func(c *commonConfig) { c.rateLimitWait = ptr(time.Duration(0)) }, false},
		{"negative rate-limit retries", func(c *commonConfig) { c.rateLimitRetries = ptr(-1) }, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := validConfig()
			c.mutate(&cfg)
			err := cfg.validate()
			if (err != nil) != c.wantErr {
				t.Fatalf("validate() error = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}
