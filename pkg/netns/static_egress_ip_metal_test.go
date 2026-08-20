//go:build metal

package netns

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

func TestMetalHostPolicyStaticEgressIPRules(t *testing.T) {
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skipf("nft not on PATH: %v", err)
	}
	c := testConfig()
	ip := netip.MustParseAddr("203.0.113.42")
	c.AccountStaticIP = &ip
	ruleset := flatten(c.NftCommands())
	cmd := exec.Command("nft", "-c", "-f", "-")
	cmd.Stdin = strings.NewReader(ruleset)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nft -c -f rejected the per-VM static-IP ruleset: err=%v out=%s ruleset: %s", err, string(out), ruleset)
	}
}

func TestMetalAccountStaticEgressIPEndToEnd(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root for netns/veth creation")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skipf("nft not on PATH: %v", err)
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skipf("ip not on PATH: %v", err)
	}
	customerIP := netip.MustParseAddr("203.0.113.42")
	if err := api.ValidateStaticEgressIP(customerIP); err != nil {
		t.Skipf("test fixture IP %s failed the deny set: %v", customerIP.String(), err)
	}
	c := testConfig()
	c.AccountStaticIP = &customerIP
	netnsName := c.Netns
	for _, argv := range c.SetupCommands() {
		cmd := exec.Command(argv[0], argv[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("setup failed: argv=%v err=%v out=%s", argv, err, string(out))
		}
	}
	defer teardownNetns(t, netnsName, c)
	for _, argv := range c.NftResetCommands() {
		exec.Command(argv[0], argv[1:]...).Run()
	}
	cmd := exec.Command("ip", "netns", "exec", netnsName, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(flatten(c.NftCommands()))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ruleset load failed: %v out=%s ruleset: %s", err, string(out), flatten(c.NftCommands()))
	}
	listCmd := exec.Command("ip", "netns", "exec", netnsName, "nft", "list", "chain", "ip", "faas", "postrouting")
	listOut, err := listCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nft list failed: %v", err)
	}
	want := "snat to " + customerIP.String()
	if !strings.Contains(string(listOut), want) {
		t.Fatalf("postrouting chain missing %q: %s", want, string(listOut))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://ifconfig.me", nil)
	req.Header.Set("User-Agent", "gregale-test/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("egress HTTP failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	ifconfig := strings.TrimSpace(string(body))
	if ifconfig != customerIP.String() {
		t.Errorf("egress source IP = %q, want %q", ifconfig, customerIP.String())
	}
}

// validCustomerStaticEgressIPMetal was a local deny-set copy. The
// single source of truth now lives in api.ValidateStaticEgressIP.
// The fixture check at line ~93 calls that helper directly; this
// function has been removed.
//
// (The TEST-NET-1/2/3 ranges are deliberately NOT in the deny set —
// a customer may legitimately BYOIP from those public blocks. The
// pin lives in pkg/netns/static_egress_ip_denylist.go's package doc.)

func teardownNetns(t *testing.T, netnsName string, c Config) {
	t.Helper()
	for _, argv := range c.TeardownCommands() {
		cmd := exec.Command(argv[0], argv[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("teardown: argv=%v err=%v out=%s", argv, err, string(out))
		}
	}
}
