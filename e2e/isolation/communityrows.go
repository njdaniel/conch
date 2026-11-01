package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/njdaniel/conch/pkg/schema"
)

// The community model's rows (issue #981, I3a): the creation oracle's
// bound, and A's public channel, which the private register leaves out.

// checkCreateRefusedAudited is the oracle row's check: B's collision with
// A's private channel's name is in the audit log as a channel_create_refused
// of the prober's with no detail, the row the daily attempt bound counts,
// and no audit row of the prober's carries the name.
func checkCreateRefusedAudited(w *world, _ api) error {
	status, body, err := w.op.do(http.MethodGet, "/v1/audit", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("the operator reads the audit log: status %d", status)
	}
	actor := fmt.Sprintf("principal:%d", w.prober.id)
	name := aChannel(w).name
	refused := false
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var e schema.AuditEventV1
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return err
		}
		if e.Actor != actor {
			continue
		}
		if strings.Contains(e.Subject+e.Detail, name) {
			return fmt.Errorf("the audit row %s of %s carries A's channel's name", e.Action, actor)
		}
		if e.Action == "channel_create_refused" && e.Detail == "" {
			refused = true
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if !refused {
		return fmt.Errorf("no channel_create_refused of %s in the audit log: the collision was not counted", actor)
	}
	return nil
}

// publicA has ann make A's public channel through POST /v1/channels and
// write in it (design §8, the private register). Its name and what is
// written there carry the public tag, not A's: they are not markers, as a
// public channel is what anyone may find once the directory lists it. A's
// private channels, what is written in them and A's people stay markers.
func (w *world) publicA() error {
	ann := w.a.people[0]
	var resp schema.CreateChannelResponseV1
	req := schema.CreateChannelRequestV1{Name: w.uName("square"), Visibility: schema.ChannelPublic}
	if err := ann.api.call(http.MethodPost, "/v1/channels", req, &resp); err != nil {
		return err
	}
	if !resp.Channel.Manages || resp.Channel.Visibility != schema.ChannelPublic {
		return fmt.Errorf("ann's public channel = %+v, want public and hers to manage", resp.Channel)
	}
	w.a.public, w.a.publicID = resp.Channel.Name, resp.Channel.ID
	var msg schema.PostMessageResponseV2
	return ann.api.call(http.MethodPost, "/v2/channels/"+w.a.public+"/messages", schema.PostMessageRequestV2{Body: w.uName("word-public")}, &msg)
}

// checkDirectory is the directory row's check (issue #982): B's member's
// listing holds A's public channel, not joined and with no count, and none
// of A's private channels; no row carries a member count.
func checkDirectory(w *world, _ api, body []byte) error {
	var resp schema.ChannelDirectoryResponseV1
	if err := json.Unmarshal(body, &resp); err != nil {
		return err
	}
	public := false
	for _, c := range resp.Channels {
		if c.Members != nil {
			return fmt.Errorf("the member's directory counts the members of channel %d", c.ID)
		}
		if slices.Contains(w.a.chanIDs, c.ID) {
			return fmt.Errorf("the private channel %d of A's is in B's directory", c.ID)
		}
		if c.ID == w.a.publicID {
			if c.Joined || c.Visibility != schema.ChannelPublic {
				return fmt.Errorf("the public channel of A's in B's directory = %+v, want public and not joined", c)
			}
			public = true
		}
	}
	if !public {
		return fmt.Errorf("the public channel %d of A's is not in B's directory", w.a.publicID)
	}
	return nil
}
