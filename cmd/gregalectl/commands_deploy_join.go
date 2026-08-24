package main

// Provider-neutral node adoption. A provider creates a machine; this command
// turns that machine into a manifest-declared Gregale compute node without
// editing the repository or teaching the CLI about GCP, Hetzner, OVH, or any
// other infrastructure API.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/onebox-faas/faas/pkg/manifest"
	"github.com/onebox-faas/faas/pkg/pki"
)

type deployJoinOptions struct {
	ManifestFile       string
	Node               string
	SSHHost            string
	SSHUser            string
	SSHPort            int
	SSHKey             string
	ReleaseTarball     string
	BootstrapBinary    string
	CosignBinary       string
	PKISource          string
	SignKeySource      string
	VerifyKeySource    string
	ComputeDBEnvSource string
	AnsibleVarsFile    string
	RepoRoot           string
	SkipFleetPreflight bool
	DryRun             bool
	Yes                bool
	JSON               bool
}

type deployJoinReport struct {
	Node           string   `json:"node"`
	DatabaseNode   string   `json:"database_node"`
	SSHHost        string   `json:"ssh_host"`
	ManifestFile   string   `json:"manifest_file"`
	ReleaseGitSHA  string   `json:"release_git_sha"`
	FleetPreflight bool     `json:"fleet_preflight"`
	Applied        bool     `json:"applied"`
	Steps          []string `json:"steps"`
}

// ansiblePlaybookRunner is a process seam for CLI tests. The production path
// streams Ansible's output so the operator sees exactly which phase failed.
var ansiblePlaybookRunner = defaultAnsiblePlaybookRunner

func defaultAnsiblePlaybookRunner(ctx context.Context, workingDir string, args []string) error {
	cmd := exec.CommandContext(ctx, "ansible-playbook", args...)
	cmd.Dir = workingDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func cmdDeployJoinNode(args []string) int {
	fs := flag.NewFlagSet("deploy join-node", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	manifestFile := fs.String("manifest-file", "", "split-box manifest containing the new compute-only node (required)")
	node := fs.String("node", "", "manifest host name to adopt (required)")
	sshHost := fs.String("ssh-host", "", "SSH address of the already-created machine (required; provider boundary)")
	sshUser := fs.String("ssh-user", "root", "SSH user for the adopted machine")
	sshPort := fs.Int("ssh-port", 22, "SSH port for the adopted machine")
	sshKey := fs.String("ssh-key", "", "optional SSH private key used by Ansible")
	releaseTarball := fs.String("release-tarball", "", "signed release.tar.gz (required for apply)")
	bootstrapBinary := fs.String("bootstrap-binary", "", "Linux/amd64 gregalectl used before the release is installed (required for apply)")
	cosignBinary := fs.String("cosign-binary", "", "cosign verifier binary staged on the adopted host (required for apply)")
	pkiSource := fs.String("pki-dir", "", "fleet PKI directory containing ca/ and the compute-only leaves (required for apply)")
	signKey := fs.String("sign-key", "", "image-signing private key (required for apply)")
	verifyKey := fs.String("verify-key", "", "image-signing public key (required for apply)")
	computeDBEnv := fs.String("compute-db-env", "", "root-only compute-db.env source (required for apply)")
	ansibleVars := fs.String("ansible-vars-file", "", "optional provider/overlay Ansible vars file")
	repoRoot := fs.String("repo-root", "", "path to the faas repository (default: inferred from gregalectl)")
	skipPreflight := fs.Bool("skip-fleet-preflight", false, "skip the complete-fleet preflight (only for a previously validated fleet)")
	dryRun := fs.Bool("dry-run", false, "validate and print the adoption plan without contacting the host")
	yes := fs.Bool("yes", false, "approve the remote adoption")
	jsonOut := fs.Bool("json", false, "emit structured JSON to stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "gregalectl deploy join-node: unexpected positional argument")
		return 2
	}

	opts := deployJoinOptions{
		ManifestFile:       *manifestFile,
		Node:               *node,
		SSHHost:            *sshHost,
		SSHUser:            *sshUser,
		SSHPort:            *sshPort,
		SSHKey:             *sshKey,
		ReleaseTarball:     *releaseTarball,
		BootstrapBinary:    *bootstrapBinary,
		CosignBinary:       *cosignBinary,
		PKISource:          *pkiSource,
		SignKeySource:      *signKey,
		VerifyKeySource:    *verifyKey,
		ComputeDBEnvSource: *computeDBEnv,
		AnsibleVarsFile:    *ansibleVars,
		RepoRoot:           *repoRoot,
		SkipFleetPreflight: *skipPreflight,
		DryRun:             *dryRun,
		Yes:                *yes,
		JSON:               *jsonOut || jsonOutput,
	}
	if opts.RepoRoot == "" {
		opts.RepoRoot = defaultRepoRoot()
	}

	report, err := deployJoinValidate(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gregalectl deploy join-node: %v\n", err)
		return 1
	}
	if opts.DryRun {
		return emitDeployJoinReport(report, false, opts.JSON)
	}
	if !opts.Yes {
		fmt.Fprintln(os.Stderr, "gregalectl deploy join-node: this will bootstrap and start services on the remote host")
		fmt.Fprintln(os.Stderr, "Re-run with --yes to proceed.")
		return 2
	}

	code, err := deployJoinApply(&opts, &report)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gregalectl deploy join-node: %v\n", err)
		return code
	}
	return emitDeployJoinReport(report, true, opts.JSON)
}

func deployJoinValidate(opts deployJoinOptions) (deployJoinReport, error) {
	report := deployJoinReport{
		Node:           opts.Node,
		DatabaseNode:   canonicalComputeNodeName(opts.Node, roleComputeOnly),
		SSHHost:        opts.SSHHost,
		ManifestFile:   opts.ManifestFile,
		FleetPreflight: !opts.SkipFleetPreflight,
		Steps: []string{
			"validate manifest and locate the compute-only host",
			"generate an ephemeral Ansible inventory without changing git",
			"adopt the provider SSH target for this host only",
			"run complete-fleet preflight unless explicitly skipped",
			"stage trust material, signed release assets, and manifest",
			"converge the production compute-only Ansible role",
			"install the signed release while the database row remains drained",
			"render configuration, initialize host identity, and start services",
			"wait for sockets, gateway, and systemd readiness",
			"activate the compute row as the final step",
		},
	}
	if opts.ManifestFile == "" {
		return report, errors.New("--manifest-file is required")
	}
	if opts.Node == "" {
		return report, errors.New("--node is required")
	}
	if opts.SSHHost == "" {
		return report, errors.New("--ssh-host is required")
	}
	if opts.SSHUser == "" {
		// Keep direct callers and programmatic tests aligned with the
		// flag parser's production default.
		opts.SSHUser = "root"
	}
	if opts.SSHPort == 0 {
		opts.SSHPort = 22
	}
	if opts.SSHPort < 1 || opts.SSHPort > 65535 {
		return report, fmt.Errorf("--ssh-port %d is outside 1..65535", opts.SSHPort)
	}
	m, err := manifest.Load(opts.ManifestFile)
	if err != nil {
		return report, err
	}
	if errs := m.Validate(); errs != nil {
		return report, fmt.Errorf("invalid manifest: %w", errs)
	}
	var hostFound bool
	for _, host := range m.Fleet.Hosts {
		if host.Name != opts.Node {
			continue
		}
		hostFound = true
		if host.Role != roleComputeOnly {
			return report, fmt.Errorf("manifest host %q has role %q; join-node requires compute-only", opts.Node, host.Role)
		}
	}
	if !hostFound {
		return report, fmt.Errorf("manifest does not declare host %q", opts.Node)
	}
	if m.Release.GitSHA == "" {
		return report, errors.New("manifest release.git_sha is empty")
	}
	report.ReleaseGitSHA = m.Release.GitSHA
	if opts.RepoRoot == "" {
		opts.RepoRoot = defaultRepoRoot()
	}
	if _, err := os.Stat(filepath.Join(opts.RepoRoot, "deploy/ansible/node_join.yml")); err != nil {
		return report, fmt.Errorf("repo root %q is missing deploy/ansible/node_join.yml: %w", opts.RepoRoot, err)
	}
	if opts.DryRun {
		return report, nil
	}
	for name, path := range map[string]string{
		"release-tarball":  opts.ReleaseTarball,
		"bootstrap-binary": opts.BootstrapBinary,
		"cosign-binary":    opts.CosignBinary,
		"pki-dir":          opts.PKISource,
		"sign-key":         opts.SignKeySource,
		"verify-key":       opts.VerifyKeySource,
		"compute-db-env":   opts.ComputeDBEnvSource,
	} {
		if path == "" {
			return report, fmt.Errorf("--%s is required for apply", name)
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			return report, fmt.Errorf("--%s: %w", name, statErr)
		}
		if name != "pki-dir" && !info.Mode().IsRegular() {
			return report, fmt.Errorf("--%s must be a regular file", name)
		}
	}
	if info, err := os.Stat(opts.BootstrapBinary); err == nil && info.Mode()&0o111 == 0 {
		return report, fmt.Errorf("--bootstrap-binary is not executable: %s", opts.BootstrapBinary)
	}
	if info, err := os.Stat(opts.CosignBinary); err == nil && info.Mode()&0o111 == 0 {
		return report, fmt.Errorf("--cosign-binary is not executable: %s", opts.CosignBinary)
	}
	for _, role := range pki.RolesForBox(roleComputeOnly) {
		cert, key := pki.LeafPaths(opts.PKISource, role)
		if _, err := os.Stat(cert); err != nil {
			return report, fmt.Errorf("--pki-dir missing %s: %w", cert, err)
		}
		if _, err := os.Stat(key); err != nil {
			return report, fmt.Errorf("--pki-dir missing %s: %w", key, err)
		}
	}
	caCert, caKey := pki.CARoot(opts.PKISource)
	for _, path := range []string{caCert, caKey} {
		if _, err := os.Stat(path); err != nil {
			return report, fmt.Errorf("--pki-dir missing %s: %w", path, err)
		}
	}
	if !hasDatabaseURLLine(opts.ComputeDBEnvSource) {
		return report, errors.New("--compute-db-env must contain DATABASE_URL")
	}
	if opts.AnsibleVarsFile != "" {
		if _, err := os.Stat(opts.AnsibleVarsFile); err != nil {
			return report, fmt.Errorf("--ansible-vars-file: %w", err)
		}
	}
	if _, err := releaseAssetPath(opts.ReleaseTarball, releaseSigName); err != nil {
		return report, err
	}
	if _, err := releaseAssetPath(opts.ReleaseTarball, releaseSBOMName); err != nil {
		return report, err
	}
	return report, nil
}

func deployJoinApply(opts *deployJoinOptions, report *deployJoinReport) (int, error) {
	if opts.RepoRoot == "" {
		opts.RepoRoot = defaultRepoRoot()
	}
	ansibleDir := filepath.Join(opts.RepoRoot, "deploy/ansible")
	tempRoot, err := os.MkdirTemp("", "gregale-node-join-")
	if err != nil {
		return 3, fmt.Errorf("create temporary inventory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempRoot) }()

	m, err := manifest.Load(opts.ManifestFile)
	if err != nil {
		return 1, err
	}
	files, err := renderManifestAnsibleFiles(m, tempRoot)
	if err != nil {
		return 1, fmt.Errorf("render temporary inventory: %w", err)
	}
	for i := range files {
		if filepath.Base(files[i].Path) == opts.Node+".yml" {
			files[i].Body = overrideJoinHostVars(files[i].Body, opts)
		}
		if err := writeGeneratedAnsibleFile(files[i].Path, files[i].Body, true); err != nil {
			return 3, fmt.Errorf("write temporary inventory: %w", err)
		}
	}

	nodeKeyDir := filepath.Join(tempRoot, "node-key")
	if err := os.MkdirAll(nodeKeyDir, 0o700); err != nil {
		return 3, fmt.Errorf("create temporary node key directory: %w", err)
	}
	priv, pub, err := generateNodeKeyPEM()
	if err != nil {
		return 3, err
	}
	nodeKeySource := filepath.Join(nodeKeyDir, "node.key")
	nodePubSource := filepath.Join(nodeKeyDir, "node.pub")
	if err := os.WriteFile(nodeKeySource, priv, 0o400); err != nil {
		return 3, fmt.Errorf("write temporary node key: %w", err)
	}
	if err := os.WriteFile(nodePubSource, pub, 0o444); err != nil {
		return 3, fmt.Errorf("write temporary node public key: %w", err)
	}

	signature, err := releaseAssetPath(opts.ReleaseTarball, releaseSigName)
	if err != nil {
		return 3, err
	}
	sbom, err := releaseAssetPath(opts.ReleaseTarball, releaseSBOMName)
	if err != nil {
		return 3, err
	}
	vars := map[string]any{
		"faas_join_inventory_name":           opts.Node,
		"faas_join_database_node":            report.DatabaseNode,
		"faas_join_release_git_sha":          report.ReleaseGitSHA,
		"faas_join_manifest_source":          opts.ManifestFile,
		"faas_join_bootstrap_binary_source":  opts.BootstrapBinary,
		"faas_join_cosign_binary_source":     opts.CosignBinary,
		"faas_join_pki_source":               opts.PKISource,
		"faas_join_sign_key_source":          opts.SignKeySource,
		"faas_join_verify_key_source":        opts.VerifyKeySource,
		"faas_join_compute_db_env_source":    opts.ComputeDBEnvSource,
		"faas_join_node_key_source":          nodeKeySource,
		"faas_join_node_pub_source":          nodePubSource,
		"faas_join_release_tarball_source":   opts.ReleaseTarball,
		"faas_join_release_signature_source": signature,
		"faas_join_release_sbom_source":      sbom,
	}
	varsPath := filepath.Join(tempRoot, "join-vars.json")
	body, err := json.Marshal(vars)
	if err != nil {
		return 3, fmt.Errorf("encode Ansible variables: %w", err)
	}
	if err := os.WriteFile(varsPath, body, 0o600); err != nil {
		return 3, fmt.Errorf("write Ansible variables: %w", err)
	}

	common := []string{"-i", filepath.Join(tempRoot, "inventory", "hosts.ini")}
	if opts.AnsibleVarsFile != "" {
		common = append(common, "-e", "@"+opts.AnsibleVarsFile)
	}
	common = append(common, "-e", "@"+varsPath)
	if !opts.SkipFleetPreflight {
		preflightArgs := append(append([]string{}, common...), filepath.Join(ansibleDir, "preflight.yml"))
		if err := ansiblePlaybookRunner(context.Background(), ansibleDir, preflightArgs); err != nil {
			return 3, fmt.Errorf("fleet preflight: %w", err)
		}
	}
	joinArgs := append(append([]string{}, common...), "--limit", opts.Node, filepath.Join(ansibleDir, "node_join.yml"))
	if err := ansiblePlaybookRunner(context.Background(), ansibleDir, joinArgs); err != nil {
		return 3, fmt.Errorf("node adoption: %w", err)
	}
	report.Applied = true
	return 0, nil
}

func overrideJoinHostVars(body []byte, opts *deployJoinOptions) []byte {
	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "ansible_host:") {
			lines[i] = "ansible_host: " + yamlQuote(opts.SSHHost)
		}
	}
	lines = append(lines,
		"ansible_user: "+yamlQuote(opts.SSHUser),
		"ansible_port: "+strconv.Itoa(opts.SSHPort),
	)
	if opts.SSHKey != "" {
		lines = append(lines, "ansible_ssh_private_key_file: "+yamlQuote(opts.SSHKey))
	}
	return []byte(strings.Join(lines, "\n"))
}

func hasDatabaseURLLine(path string) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "DATABASE_URL=") && len(strings.TrimSpace(strings.TrimPrefix(trimmed, "DATABASE_URL="))) > 0 {
			return true
		}
	}
	return false
}

func releaseAssetPath(tarballPath, name string) (string, error) {
	candidates := []string{
		filepath.Join(filepath.Dir(tarballPath), name),
		tarballPath + "." + strings.TrimPrefix(name, "release."),
	}
	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path, nil
		}
	}
	return "", fmt.Errorf("release tarball is missing %s (tried %s)", name, strings.Join(candidates, ", "))
}

func emitDeployJoinReport(report deployJoinReport, applied bool, jsonOut bool) int {
	report.Applied = applied
	if jsonOut {
		jsonEmit(os.Stdout, report)
		return 0
	}
	state := "plan"
	if applied {
		state = "active"
	}
	_, _ = fmt.Fprintf(os.Stdout, "deploy join-node: %s node=%s release=%s ssh=%s\n", state, report.DatabaseNode, report.ReleaseGitSHA, report.SSHHost)
	for i, step := range report.Steps {
		_, _ = fmt.Fprintf(os.Stdout, "  %d. %s\n", i+1, step)
	}
	return 0
}
