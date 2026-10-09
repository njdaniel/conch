package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func TestBootstrapOperatorExistingCredentials(t *testing.T) {
	tests := []struct {
		name       string
		extraArgs  []string
		wantNotice string
		wantLive   bool
	}{
		{"revokes by default and reports the count", nil, "revoked 2 existing credential(s)", false},
		{"keep flag leaves them and stays quiet", []string{"--keep-existing-credentials"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			st, err := store.Open(ctx, filepath.Join(dir, "conch.db"))
			if err != nil {
				t.Fatal(err)
			}
			agent, err := st.CreatePrincipal(ctx, store.PrincipalAgent, "old")
			if err != nil {
				t.Fatal(err)
			}
			var old []string
			for _, label := range []string{"a", "b"} {
				_, tok, err := st.CreateCredential(ctx, "system", agent.ID, label, nil)
				if err != nil {
					t.Fatal(err)
				}
				old = append(old, tok)
			}
			_ = st.Close()

			var out, errOut bytes.Buffer
			args := append([]string{"--data", dir, "--name", "root"}, tt.extraArgs...)
			if err := runBootstrapOperator(args, &out, &errOut); err != nil {
				t.Fatal(err)
			}
			if tt.wantNotice != "" && !strings.Contains(errOut.String(), tt.wantNotice) {
				t.Errorf("stderr = %q, want it to contain %q", errOut.String(), tt.wantNotice)
			}
			if tt.wantNotice == "" && strings.Contains(errOut.String(), "revoked") {
				t.Errorf("stderr = %q, want no revocation notice", errOut.String())
			}
			if strings.Count(out.String(), "\n") != 1 {
				t.Errorf("stdout must be the token only, got %q", out.String())
			}

			st, err = store.Open(ctx, filepath.Join(dir, "conch.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			for _, tok := range old {
				_, err := st.ResolveCredential(ctx, tok)
				if (err == nil) != tt.wantLive {
					t.Errorf("old token resolves = %v, want %v", err == nil, tt.wantLive)
				}
			}
			if _, err := st.ResolveCredential(ctx, strings.TrimSpace(out.String())); err != nil {
				t.Errorf("operator token does not resolve: %v", err)
			}
		})
	}
}

func TestBootstrapOperatorNotices(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh directory says a database was created", func(t *testing.T) {
		var out, errOut bytes.Buffer
		dir := filepath.Join(t.TempDir(), "new")
		if err := runBootstrapOperator([]string{"--data", dir, "--name", "root"}, &out, &errOut); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(errOut.String(), "a new one was created") {
			t.Errorf("stderr = %q, want the new-database note", errOut.String())
		}
		if strings.Contains(errOut.String(), strings.TrimSpace(out.String())) {
			t.Error("stderr contains the token")
		}
	})

	t.Run("existing database: hooks deleted, manifests reported, no creation note", func(t *testing.T) {
		dir := t.TempDir()
		st, err := store.Open(ctx, filepath.Join(dir, "conch.db"))
		if err != nil {
			t.Fatal(err)
		}
		channel, err := st.CreateChannel(ctx, "ops")
		if err != nil {
			t.Fatal(err)
		}
		agent, err := st.CreatePrincipal(ctx, store.PrincipalAgent, "old")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.PutAgentManifest(ctx, "system", agent.ID, schema.PutAgentManifestRequestV1{DisplayName: "Old", Tier: schema.AgentTierC}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateHook(ctx, "pre-operator-hook", channel.ID, agent.ID); err != nil {
			t.Fatal(err)
		}
		_ = st.Close()

		var out, errOut bytes.Buffer
		if err := runBootstrapOperator([]string{"--data", dir, "--name", "root"}, &out, &errOut); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"deleted 1 existing webhook hook(s)", "1 agent manifest(s) already exist"} {
			if !strings.Contains(errOut.String(), want) {
				t.Errorf("stderr = %q, want it to contain %q", errOut.String(), want)
			}
		}
		for _, unwanted := range []string{"a new one was created", "pre-operator-hook"} {
			if strings.Contains(errOut.String(), unwanted) {
				t.Errorf("stderr = %q, must not contain %q", errOut.String(), unwanted)
			}
		}
		st, err = store.Open(ctx, filepath.Join(dir, "conch.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		if _, err := st.HookByToken(ctx, "pre-operator-hook"); err == nil {
			t.Error("the pre-operator hook still resolves")
		}
	})
}
