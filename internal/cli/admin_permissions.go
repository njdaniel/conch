package cli

import (
	"context"
	"flag"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/njdaniel/conch/pkg/schema"
)

// `conch admin permissions show|set|preset` (ADR-014, issue #1022): the
// console's permission controls, as a client of GET and PUT /v1/permissions.
// The CLI decides nothing: it names no more than the schema's own checks, and
// the server refuses whatever it will not do.

// permissionDescriptions are the permissions whose effect is worth saying in
// full wherever the document is shown (ADR-014, design §4). The words are the
// design's.
var permissionDescriptions = map[schema.Permission]string{
	schema.PermActAsMembers: "can sign in as, and post as, any member who isn't an owner",
	schema.PermReadAuditLog: "sees every member and every private channel's name in the audit trail",
}

// warningTexts say what a warning of the document means. A warning this
// client does not know is shown by its name.
var warningTexts = map[schema.PermissionsWarning]string{
	schema.PermissionsWarningDefaultChannelExposesMembers: "members cannot see the member directory, but everyone in a default channel sees everyone else there",
	schema.PermissionsWarningOpenSignupOn:                 "members cannot make member links, but open sign-up still lets anyone in through the address",
}

// permissionsOpts holds the flags of `permissions set`.
type permissionsOpts struct {
	members, operators string
}

func bindPermissionsSet(fs *flag.FlagSet, o *adminOpts) {
	fs.StringVar(&o.permissions.members, "members", "", "on or off: the members' cell")
	fs.StringVar(&o.permissions.operators, "operators", "", "on or off: the operators' cell (owners only)")
}

// adminPermissionsShow prints the permission table, the preset the member
// column matches, and the warnings.
func adminPermissionsShow(ctx context.Context, r *adminRun) error {
	doc, err := r.client.GetPermissions(ctx)
	if err != nil {
		return err
	}
	if r.opts.asJSON {
		return r.printJSON(doc)
	}
	return writePermissions(r.stdout, doc)
}

// writePermissions prints the document. Every string in it came from the
// server and is made safe for one line.
func writePermissions(w interface{ Write([]byte) (int, error) }, doc schema.PermissionsResponseV1) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "revision: %d\n", doc.Revision)
	tw := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PERMISSION\tMEMBERS\tOPERATORS\tFIXED")
	for _, row := range doc.Rows {
		var fixed []string
		if row.MemberFixed {
			fixed = append(fixed, "members")
		}
		if row.OperatorFixed {
			fixed = append(fixed, "operators")
		}
		fixedText := "-"
		if len(fixed) > 0 {
			fixedText = strings.Join(fixed, ", ")
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", safeLine(string(row.Permission)), onOffText(row.Member), onOffText(row.Operator), fixedText)
	}
	_ = tw.Flush()
	for _, row := range doc.Rows {
		if text, ok := permissionDescriptions[row.Permission]; ok {
			fmt.Fprintf(&sb, "%s: %s\n", safeLine(string(row.Permission)), text)
		}
	}
	if doc.Preset == "" {
		sb.WriteString("preset: none (the member column matches no preset)\n")
	} else {
		fmt.Fprintf(&sb, "preset: %s\n", safeLine(string(doc.Preset)))
	}
	if len(doc.Editable) > 0 {
		names := make([]string, len(doc.Editable))
		for i, e := range doc.Editable {
			names[i] = safeLine(string(e))
		}
		fmt.Fprintf(&sb, "you may change: %s\n", strings.Join(names, ", "))
	} else {
		sb.WriteString("you may change: nothing\n")
	}
	for _, warn := range doc.Warnings {
		sb.WriteString(warningLine(warn) + "\n")
	}
	_, err := w.Write([]byte(sb.String()))
	return err
}

func onOffText(b bool) string { return either(b, "on", "off") }

func warningLine(w schema.PermissionsWarning) string {
	if text, ok := warningTexts[w]; ok {
		return "warning: " + safeLine(string(w)) + ": " + text
	}
	return "warning: " + safeLine(string(w))
}

// adminPermissionsSet changes one permission's cells.
func adminPermissionsSet(ctx context.Context, r *adminRun) error {
	const cmd = "admin permissions set"
	perm := schema.Permission(r.pos[0])
	if !perm.Valid() {
		known := make([]string, len(schema.Permissions))
		for i, p := range schema.Permissions {
			known[i] = string(p)
		}
		return fmt.Errorf("cli: %s: %s is not a permission this conch knows (known: %s)", cmd, safeLine(r.pos[0]), strings.Join(known, ", "))
	}
	var changes []schema.PermissionChangeV1
	for _, c := range []struct {
		flagName string
		role     schema.PermissionRole
	}{{"members", schema.PermissionRoleMember}, {"operators", schema.PermissionRoleOperator}} {
		if !r.given[c.flagName] {
			continue
		}
		v := r.opts.permissions.members
		if c.role == schema.PermissionRoleOperator {
			v = r.opts.permissions.operators
		}
		granted, err := onOff(c.flagName, v)
		if err != nil {
			return fmt.Errorf("cli: %s: %w", cmd, err)
		}
		if granted == nil { // given empty
			return fmt.Errorf("cli: %s: --%s must be on or off", cmd, c.flagName)
		}
		changes = append(changes, schema.PermissionChangeV1{Permission: perm, Role: c.role, Granted: *granted})
	}
	if len(changes) == 0 {
		return fmt.Errorf("cli: %s: give --members or --operators (usage: conch admin permissions set [--server <url>] [--json] [--members on|off] [--operators on|off] <permission>)", cmd)
	}
	doc, err := r.client.GetPermissions(ctx)
	if err != nil {
		return err
	}
	req := schema.PutPermissionsRequestV1{Revision: doc.Revision, Changes: changes}
	// The schema's own checks, so a cell that can never be set costs no
	// request.
	if err := req.Validate(); err != nil {
		return fmt.Errorf("cli: %s: %w", cmd, err)
	}
	out, err := r.putPermissions(ctx, cmd, req)
	if err != nil {
		return err
	}
	if r.opts.asJSON {
		return r.printJSON(out)
	}
	for _, row := range out.Rows {
		if row.Permission == perm {
			if err := r.say("%s: members %s, operators %s (revision %d)\n", safeLine(string(perm)), onOffText(row.Member), onOffText(row.Operator), out.Revision); err != nil {
				return err
			}
		}
	}
	return r.sayPermissionsAftermath(out)
}

// adminPermissionsPreset sets the member column to a preset, after showing
// what it changes.
func adminPermissionsPreset(ctx context.Context, r *adminRun) error {
	const cmd = "admin permissions preset"
	preset := schema.PermissionPreset(r.pos[0])
	if !preset.Valid() {
		return fmt.Errorf("cli: %s: %s is not a preset this conch knows (known: personal, group)", cmd, safeLine(r.pos[0]))
	}
	doc, err := r.client.GetPermissions(ctx)
	if err != nil {
		return err
	}
	idx := slices.IndexFunc(doc.Presets, func(p schema.PermissionPresetV1) bool { return p.Preset == preset })
	if idx < 0 {
		return fmt.Errorf("cli: %s: the server does not define the %s preset", cmd, safeLine(string(preset)))
	}
	var diff []string
	for _, row := range doc.Rows {
		if row.MemberFixed {
			continue
		}
		if want := slices.Contains(doc.Presets[idx].Member, row.Permission); want != row.Member {
			diff = append(diff, fmt.Sprintf("  %s: members %s -> %s", safeLine(string(row.Permission)), onOffText(row.Member), onOffText(want)))
		}
	}
	if len(diff) == 0 {
		if r.opts.asJSON {
			return r.printJSON(doc)
		}
		return r.say("the member column already matches the %s preset; nothing to change\n", preset)
	}
	if !r.opts.asJSON {
		if err := r.say("the %s preset changes the member column:\n%s\n", preset, strings.Join(diff, "\n")); err != nil {
			return err
		}
	}
	if !r.opts.invite.yes {
		ok, err := r.prompt.confirm(fmt.Sprintf("Apply the %s preset?", preset))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("cli: %s: not applied", cmd)
		}
	}
	out, err := r.putPermissions(ctx, cmd, schema.PutPermissionsRequestV1{Revision: doc.Revision, Preset: preset})
	if err != nil {
		return err
	}
	if r.opts.asJSON {
		return r.printJSON(out)
	}
	if err := r.say("applied the %s preset (revision %d)\n", preset, out.Revision); err != nil {
		return err
	}
	return r.sayPermissionsAftermath(out)
}

// putPermissions sends the change. A stale revision is not retried: the
// document is read again, to say what it is now, and the person decides.
func (r *adminRun) putPermissions(ctx context.Context, cmd string, req schema.PutPermissionsRequestV1) (schema.PermissionsResponseV1, error) {
	out, err := r.client.PutPermissions(ctx, req)
	if !IsServerCode(err, schema.ErrorCodePermissionsStale) {
		return out, err
	}
	now, readErr := r.client.GetPermissions(ctx)
	if readErr != nil {
		return schema.PermissionsResponseV1{}, fmt.Errorf("cli: %s: the permissions changed since they were read; nothing was changed (and reading them again failed: %w)", cmd, readErr)
	}
	return schema.PermissionsResponseV1{}, fmt.Errorf("cli: %s: the permissions changed since they were read (revision %d, now %d); nothing was changed; look with `conch admin permissions show` and run it again", cmd, req.Revision, now.Revision)
}

// sayPermissionsAftermath prints what the document's answer adds to a write:
// its warnings, and what operators issued that outlives act_as_members.
func (r *adminRun) sayPermissionsAftermath(doc schema.PermissionsResponseV1) error {
	for _, w := range doc.Warnings {
		if err := r.say("%s\n", warningLine(w)); err != nil {
			return err
		}
	}
	if len(doc.IssuedToOthers) == 0 {
		return nil
	}
	if err := r.say("operators issued these to other people; turning act_as_members off does not end them. Revoke each you do not want:\n"); err != nil {
		return err
	}
	for _, item := range doc.IssuedToOthers {
		if err := r.say("%s\n", issuedLine(item)); err != nil {
			return err
		}
	}
	return nil
}

// issuedLine describes one credential or hook an operator issued to someone
// else, with the command that revokes it. An item with an id that cannot name
// anything, or a kind this client does not know, gets no command.
func issuedLine(item schema.IssuedByOperatorV1) string {
	who := "principal " + strconv.FormatInt(item.PrincipalID, 10) + ", issued by " + strconv.FormatInt(item.IssuedBy, 10)
	kind := safeLine(string(item.Kind))
	if item.ID <= 0 {
		return fmt.Sprintf("  %s %s (%s): no valid id to revoke", kind, strconv.FormatInt(item.ID, 10), who)
	}
	id := strconv.FormatInt(item.ID, 10)
	switch item.Kind {
	case schema.IssuedCredential:
		return fmt.Sprintf("  credential %s (%s): conch admin credential revoke %s", id, who, id)
	case schema.IssuedHook:
		// conch has no hook command; the API route is the way.
		return fmt.Sprintf("  hook %s (%s): DELETE /v1/hooks/%s (the API; conch has no hook command)", id, who, id)
	default:
		return fmt.Sprintf("  %s %s (%s): this conch does not know how to revoke it", kind, id, who)
	}
}
