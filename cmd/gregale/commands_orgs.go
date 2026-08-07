// commands_orgs.go — `gregale orgs <subcommand>` customer CLI for
// IAM-6 / ADR-061 / issue #190 (org CRUD + member management +
// invitations + ownership transfer).
//
// The org surface is the largest IAM feature behind MFA — five
// top-level subcommands, two nested dispatchers (`orgs members ...`,
// `orgs invitations ...`), plus a `transfer-ownership` leaf. The
// standalone `gregale invitations peek|accept` lives here too
// because the token-based entry points don't carry a slug context.
//
// Subcommand vocab mirrors the API verbs (the kebab-case surface in
// api/openapi.yaml, gated by the closed role vocab at
// pkg/api/orgs.go:AllowedOrgRoles). The CLI never validates the
// role locally beyond the closed-set typo check — apid re-validates
// at the boundary, and the CLI's job is to surface a clean error
// before the network round-trip.
//
// Invitation tokens are one-shot (per pkg/api/orgs.go:OrgInvitationResponse):
// the plaintext token is returned ONCE on the create call and the
// server-side row stores only its SHA-256 hash. The CLI prints the
// token to stdout immediately after the API call returns; the
// customer copies it into an email/off-channel ping. There is no
// re-fetch path — losing the token means re-inviting.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/onebox-faas/faas/pkg/api"
)

// cmdOrgs dispatches `gregale orgs <subcommand>`. The top-level
// fan-out mirrors the existing webhooks/crons dispatcher shape so
// the customer sees the same `list|create|info|rm` + nested
// namespacing across all three surfaces.
func cmdOrgs(args []string) int {
	if len(args) == 0 {
		PrintUsage(os.Stderr, "usage: gregale orgs <list|create|info|rm|members|invitations|transfer-ownership|seat-usage> [args]", "orgs")
		return 1
	}
	switch args[0] {
	case subList:
		return cmdOrgsLs(args[1:])
	case "create":
		return cmdOrgsCreate(args[1:])
	case "info":
		return cmdOrgsInfo(args[1:])
	case subRm:
		return cmdOrgsRm(args[1:])
	case "members":
		return cmdOrgsMembers(args[1:])
	case "invitations":
		return cmdOrgsInvitations(args[1:])
	case "transfer-ownership":
		return cmdOrgsTransferOwnership(args[1:])
	case "seat-usage":
		return cmdOrgsSeatUsage(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "gregale orgs: unknown subcommand %q\n", args[0])
		return 1
	}
}

// cmdOrgsMembers is the nested dispatcher for `gregale orgs members
// <list|invite|change-role|rm>`. The vocabulary mirrors the API verbs
// rather than the dashboard's "add member / edit role" labels —
// `gregale crons list` already established this posture for the
// scheduled-requests surface.
func cmdOrgsMembers(args []string) int {
	if len(args) == 0 {
		PrintUsage(os.Stderr, "usage: gregale orgs members <list|invite|change-role|rm> [args]", "orgs")
		return 1
	}
	switch args[0] {
	case subList:
		return cmdOrgsMembersLs(args[1:])
	case "invite":
		return cmdOrgsMembersInvite(args[1:])
	case "change-role":
		return cmdOrgsMembersChangeRole(args[1:])
	case subRm:
		return cmdOrgsMembersRm(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "gregale orgs members: unknown subcommand %q\n", args[0])
		return 1
	}
}

// cmdOrgsInvitations is the nested dispatcher for `gregale orgs
// invitations <list|revoke>`. The standalone `gregale invitations
// peek|accept` (token-based, no slug context) lives in
// cmdInvitations below.
func cmdOrgsInvitations(args []string) int {
	if len(args) == 0 {
		PrintUsage(os.Stderr, "usage: gregale orgs invitations <list|revoke> [args]", "orgs")
		return 1
	}
	switch args[0] {
	case subList:
		return cmdOrgsInvitationsLs(args[1:])
	case "revoke":
		return cmdOrgsInvitationsRevoke(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "gregale orgs invitations: unknown subcommand %q\n", args[0])
		return 1
	}
}

// cmdInvitations is the standalone dispatcher for `gregale invitations
// <peek|accept>`. Token-based entry points without a slug context —
// peek is unauth-friendly (the server validates the token itself),
// accept requires an authenticated session + 5-min strict step-up
// (server-side; CLI surfaces the 403 as-is).
func cmdInvitations(args []string) int {
	if len(args) == 0 {
		PrintUsage(os.Stderr, "usage: gregale invitations <peek|accept> <token>", "invitations")
		return 1
	}
	switch args[0] {
	case "peek":
		return cmdInvitationsPeek(args[1:])
	case "accept":
		return cmdInvitationsAccept(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "gregale invitations: unknown subcommand %q\n", args[0])
		return 1
	}
}

// orgRoleForInviteVocab is the invite-eligible subset mirrored from
// pkg/api.AllowedOrgMemberRolesForInvite — excludes owner because
// transfer-ownership is the only path to ownership. Used by the
// `invite` leaf only.
var orgRoleForInviteVocab = api.AllowedOrgMemberRolesForInvite

// orgRoleForPatchVocab is the change-role-eligible subset mirrored
// from pkg/api.AllowedOrgDirectPatchRoles. Same as
// orgRoleForInviteVocab per pkg/api/orgs.go:73-77 — PATCH cannot
// promote a member to owner; only transfer-ownership can.
var orgRoleForPatchVocab = api.AllowedOrgDirectPatchRoles

// --- org CRUD leaves -------------------------------------------------------

func cmdOrgsLs(args []string) int {
	fs := flag.NewFlagSet("orgs ls", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 0 {
		PrintUsage(os.Stderr, "usage: gregale orgs ls", "orgs")
		return 1
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	resp, err := client.ListOrgs(context.Background())
	if err != nil {
		return printErr("Could not list orgs", err)
	}
	if jsonOutput {
		return jsonOut(writeNDJSON(resp.Orgs))
	}
	if len(resp.Orgs) == 0 {
		PrintProgress(osStdout, "(no orgs)")
		return 0
	}
	for _, o := range resp.Orgs {
		flag := ""
		if o.Personal {
			flag = " (personal)"
		}
		fmt.Printf("%-32s %-12s %-10s %s%s\n", o.Slug, o.Plan, o.Status, o.Name, flag)
	}
	return 0
}

func cmdOrgsCreate(args []string) int {
	fs := flag.NewFlagSet("orgs create", flag.ContinueOnError)
	slug := fs.String("slug", "", "org slug (required; lowercase alphanumeric + dashes)")
	name := fs.String("name", "", "display name (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *slug == "" || *name == "" {
		PrintUsage(os.Stderr, "usage: gregale orgs create --slug <slug> --name <display>", "orgs")
		return 1
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	o, err := client.CreateOrg(context.Background(), api.CreateOrgRequest{Slug: *slug, Name: *name})
	if err != nil {
		return printErr("Could not create org", err)
	}
	if jsonOutput {
		return jsonOut(writeJSON(o))
	}
	PrintOK(osStdout, "Org created: %s (%s)", o.Slug, o.Plan)
	return 0
}

func cmdOrgsInfo(args []string) int {
	fs := flag.NewFlagSet("orgs info", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 1 {
		PrintUsage(os.Stderr, "usage: gregale orgs info <slug>", "orgs")
		return 1
	}
	slug := fs.Arg(0)
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	o, err := client.GetOrg(context.Background(), slug)
	if err != nil {
		return printErr("Could not fetch org", err)
	}
	if jsonOutput {
		return jsonOut(writeJSON(o))
	}
	fmt.Printf("slug:      %s\n", o.Slug)
	fmt.Printf("name:      %s\n", o.Name)
	fmt.Printf("plan:      %s\n", o.Plan)
	fmt.Printf("status:    %s\n", o.Status)
	fmt.Printf("personal:  %v\n", o.Personal)
	fmt.Printf("created:   %s\n", o.CreatedAt)
	fmt.Printf("updated:   %s\n", o.UpdatedAt)
	return 0
}

func cmdOrgsRm(args []string) int {
	fs := flag.NewFlagSet("orgs rm", flag.ContinueOnError)
	quiet := fs.Bool("q", false, "suppress confirmation prompt")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 1 {
		PrintUsage(os.Stderr, "usage: gregale orgs rm <slug> [-q]", "orgs")
		return 1
	}
	slug := fs.Arg(0)
	if !*quiet {
		fmt.Fprintf(os.Stderr,
			"This will soft-delete the org %q (apps + members retained; restore via dashboard).\n"+
				"Continue? [y/N] ", slug)
		var ans string
		_, _ = fmt.Scanln(&ans)
		if ans != "y" {
			fmt.Println("aborted")
			return 1
		}
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	if err := client.DeleteOrg(context.Background(), slug); err != nil {
		return printErr("Could not delete org", err)
	}
	PrintOK(osStdout, "Org %s scheduled for deletion", slug)
	return 0
}

// --- members ---------------------------------------------------------------

func cmdOrgsMembersLs(args []string) int {
	fs := flag.NewFlagSet("orgs members ls", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 1 {
		PrintUsage(os.Stderr, "usage: gregale orgs members ls <slug>", "orgs")
		return 1
	}
	slug := fs.Arg(0)
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	resp, err := client.ListOrgMembers(context.Background(), slug)
	if err != nil {
		return printErr("Could not list members", err)
	}
	if jsonOutput {
		return jsonOut(writeNDJSON(resp.Members))
	}
	for _, m := range resp.Members {
		fmt.Printf("%-36s %-32s %-12s %s\n", m.AccountID, m.Email, m.Role, m.JoinedAt)
	}
	return 0
}

func cmdOrgsMembersInvite(args []string) int {
	fs := flag.NewFlagSet("orgs members invite", flag.ContinueOnError)
	slug := fs.String("org", "", "org slug (required)")
	email := fs.String("email", "", "invitee email (required)")
	role := fs.String("role", "developer", "role: admin|developer|viewer|billing (owner is rejected)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *slug == "" || *email == "" {
		PrintUsage(os.Stderr, "usage: gregale orgs members invite --org <slug> --email <addr> [--role admin|developer|viewer|billing]", "orgs")
		return 1
	}
	if !strInSlice(*role, orgRoleForInviteVocab) {
		return printErr("Invalid --role", fmt.Errorf("must be one of %v (owner is invite-rejected); got %q", orgRoleForInviteVocab, *role))
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	resp, err := client.InviteOrgMember(context.Background(), *slug, api.InviteMemberRequest{Email: *email, Role: *role})
	if err != nil {
		return printErr("Could not create invitation", err)
	}
	if jsonOutput {
		return jsonOut(writeJSON(resp))
	}
	// One-shot token: print immediately, do NOT log elsewhere.
	// The customer copies the token into an off-channel ping.
	PrintOK(osStdout, "Invitation created for %s (role=%s)", *email, *role)
	PrintProgress(osStdout, "token: %s", resp.Token)
	PrintProgress(osStdout, "expires_at: %s", resp.ExpiresAt)
	PrintProgress(osStdout, "Send the token to the invitee; it is single-use and will NOT be shown again.")
	return 0
}

func cmdOrgsMembersChangeRole(args []string) int {
	fs := flag.NewFlagSet("orgs members change-role", flag.ContinueOnError)
	slug := fs.String("org", "", "org slug (required)")
	userID := fs.String("user", "", "user-id (required)")
	role := fs.String("role", "", "new role: admin|developer|viewer|billing (owner is rejected)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *slug == "" || *userID == "" || *role == "" {
		PrintUsage(os.Stderr, "usage: gregale orgs members change-role --org <slug> --user <user-id> --role <role>", "orgs")
		return 1
	}
	if !strInSlice(*role, orgRoleForPatchVocab) {
		return printErr("Invalid --role", fmt.Errorf("must be one of %v (owner is patch-rejected; use transfer-ownership); got %q", orgRoleForPatchVocab, *role))
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	if _, err := client.ChangeOrgMemberRole(context.Background(), *slug, *userID, api.ChangeMemberRoleRequest{Role: *role}); err != nil {
		return printErr("Could not change role", err)
	}
	PrintOK(osStdout, "Member %s role changed to %s", *userID, *role)
	return 0
}

func cmdOrgsMembersRm(args []string) int {
	fs := flag.NewFlagSet("orgs members rm", flag.ContinueOnError)
	slug := fs.String("org", "", "org slug (required)")
	userID := fs.String("user", "", "user-id (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *slug == "" || *userID == "" {
		PrintUsage(os.Stderr, "usage: gregale orgs members rm --org <slug> --user <user-id>", "orgs")
		return 1
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	if err := client.RemoveOrgMember(context.Background(), *slug, *userID); err != nil {
		return printErr("Could not remove member", err)
	}
	PrintOK(osStdout, "Member %s removed from org %s", *userID, *slug)
	return 0
}

// --- org-level invitations -------------------------------------------------

func cmdOrgsInvitationsLs(args []string) int {
	fs := flag.NewFlagSet("orgs invitations list", flag.ContinueOnError)
	slug := fs.String("org", "", "org slug (required)")
	limit := fs.Int("limit", 50, "max rows (1..200; server caps at 200)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *slug == "" {
		PrintUsage(os.Stderr, "usage: gregale orgs invitations list --org <slug> [--limit N]", "orgs")
		return 1
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	resp, err := client.ListOrgInvitations(context.Background(), *slug, "", *limit)
	if err != nil {
		return printErr("Could not list invitations", err)
	}
	if jsonOutput {
		return jsonOut(writeNDJSON(resp.Invitations))
	}
	for _, inv := range resp.Invitations {
		fmt.Printf("%-36s %-30s %-12s %-10s %s\n", inv.ID, inv.Email, inv.Role, inv.Status, inv.ExpiresAt)
	}
	return 0
}

func cmdOrgsInvitationsRevoke(args []string) int {
	fs := flag.NewFlagSet("orgs invitations revoke", flag.ContinueOnError)
	slug := fs.String("org", "", "org slug (required)")
	invID := fs.String("invitation", "", "invitation-id (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *slug == "" || *invID == "" {
		PrintUsage(os.Stderr, "usage: gregale orgs invitations revoke --org <slug> --invitation <id>", "orgs")
		return 1
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	if err := client.RevokeInvitation(context.Background(), *slug, *invID); err != nil {
		return printErr("Could not revoke invitation", err)
	}
	PrintOK(osStdout, "Invitation %s revoked", *invID)
	return 0
}

// --- ownership + seats -----------------------------------------------------

func cmdOrgsTransferOwnership(args []string) int {
	fs := flag.NewFlagSet("orgs transfer-ownership", flag.ContinueOnError)
	slug := fs.String("org", "", "org slug (required)")
	to := fs.String("to", "", "user-id of the new owner (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *slug == "" || *to == "" {
		PrintUsage(os.Stderr, "usage: gregale orgs transfer-ownership --org <slug> --to <user-id>", "orgs")
		return 1
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	if _, err := client.TransferOrgOwnership(context.Background(), *slug, api.TransferOwnershipRequest{NewOwnerAccountID: *to}); err != nil {
		return printErr("Could not transfer ownership", err)
	}
	PrintOK(osStdout, "Ownership of %s transferred to %s. The previous owner is now admin.", *slug, *to)
	return 0
}

func cmdOrgsSeatUsage(args []string) int {
	fs := flag.NewFlagSet("orgs seat-usage", flag.ContinueOnError)
	slug := fs.String("org", "", "org slug (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *slug == "" {
		PrintUsage(os.Stderr, "usage: gregale orgs seat-usage --org <slug>", "orgs")
		return 1
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	resp, err := client.GetOrgSeatUsage(context.Background(), *slug)
	if err != nil {
		return printErr("Could not fetch seat usage", err)
	}
	if jsonOutput {
		return jsonOut(writeJSON(resp))
	}
	fmt.Printf("plan:  %s\n", resp.Plan)
	fmt.Printf("used:  %d\n", resp.Used)
	fmt.Printf("limit: %d\n", resp.Limit)
	return 0
}

// --- standalone invitations (token-based, no slug context) ----------------

func cmdInvitationsPeek(args []string) int {
	fs := flag.NewFlagSet("invitations peek", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 1 {
		PrintUsage(os.Stderr, "usage: gregale invitations peek <token>", "invitations")
		return 1
	}
	token := fs.Arg(0)
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	resp, err := client.PeekInvitation(context.Background(), token)
	if err != nil {
		return printErr("Could not peek invitation", err)
	}
	if jsonOutput {
		return jsonOut(writeJSON(resp))
	}
	fmt.Printf("org:        %s\n", resp.OrgSlug)
	fmt.Printf("email:      %s\n", resp.Email)
	fmt.Printf("role:       %s\n", resp.Role)
	fmt.Printf("status:     %s\n", resp.Status)
	fmt.Printf("expires_at: %s\n", resp.ExpiresAt)
	return 0
}

func cmdInvitationsAccept(args []string) int {
	fs := flag.NewFlagSet("invitations accept", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 1 {
		PrintUsage(os.Stderr, "usage: gregale invitations accept <token>", "invitations")
		return 1
	}
	token := fs.Arg(0)
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	m, err := client.AcceptInvitation(context.Background(), token)
	if err != nil {
		return printErr("Could not accept invitation", err)
	}
	if jsonOutput {
		return jsonOut(writeJSON(m))
	}
	PrintOK(osStdout, "Joined %s as %s", m.AccountID, m.Role)
	return 0
}
