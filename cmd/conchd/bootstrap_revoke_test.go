package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
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
