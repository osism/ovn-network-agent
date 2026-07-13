package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installFakeNft writes a small POSIX shell script onto PATH that captures
// each invocation's args and stdin into the returned dir and exits non-zero.
// Tests use it to assert that applyNftRuleset surfaces the error and reissues
// the identical ruleset on the next call (no caching of partial state).
func installFakeNft(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
N=$(cat "` + dir + `/count" 2>/dev/null || echo 0)
N=$((N + 1))
echo "$N" > "` + dir + `/count"
echo "$@" > "` + dir + `/args.$N"
cat > "` + dir + `/stdin.$N"
echo "fake nft refused ruleset (invocation $N)" >&2
exit 1
`
	bin := filepath.Join(dir, "nft")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake nft: %v", err)
	}
	// Prepend dir so the fake takes precedence over a real nft on the host.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// readNftStdin returns the captured stdin for invocation n (1-based).
func readNftStdin(t *testing.T, dir string, n int) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "stdin."+itoa(n)))
	if err != nil {
		t.Fatalf("read stdin.%d: %v", n, err)
	}
	return string(data)
}

// itoa avoids pulling strconv into the test for a single int-to-string.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// applyNftRuleset returns the wrapped exec error when nft exits non-zero, and
// because the RouteManager keeps no per-call ruleset state, a follow-up call
// emits an identical ruleset rather than skipping or mutating the prior one.
// This is the reapply contract the reconcile loop relies on: a transient
// `nft -f -` failure must not bake stale state into the next attempt.
func TestApplyNftRulesetReissuesIdenticalRulesetAfterFailure(t *testing.T) {
	dir := installFakeNft(t)

	rm := &RouteManager{cfg: Config{PortForwardEnabled: true, PortForwardDev: "loopback1", PortForwardCTZone: 64000, PortForwards: []PortForwardVIP{
		{
			VIP:       "198.51.100.10",
			ManageVIP: true,
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100", DestPort: 8080},
			},
		},
	}}}

	// First call: nft fails → wrapped error returned.
	err := rm.applyNftRuleset(nil, nil)
	if err == nil {
		t.Fatal("applyNftRuleset: expected error from failing nft, got nil")
	}
	if !strings.Contains(err.Error(), "nft apply ruleset") {
		t.Errorf("error %q does not mention nft apply ruleset wrapper", err)
	}

	// Second call must re-issue the same ruleset — no cached "we already
	// tried this" suppression, no half-applied state from the failed run.
	err2 := rm.applyNftRuleset(nil, nil)
	if err2 == nil {
		t.Fatal("applyNftRuleset: expected error on re-issue, got nil")
	}

	// The fake nft captures both an `nft list table ...` probe and an
	// `nft -f -` apply per call. Count the two -f - invocations and assert
	// their stdin payloads match.
	var applyStdins []string
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range dirEntries {
		if !strings.HasPrefix(e.Name(), "args.") {
			continue
		}
		argsBytes, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if strings.TrimSpace(string(argsBytes)) != "-f -" {
			continue
		}
		n := strings.TrimPrefix(e.Name(), "args.")
		applyStdins = append(applyStdins, readNftStdin(t, dir, atoiOrFail(t, n)))
	}
	if len(applyStdins) != 2 {
		t.Fatalf("expected 2 `nft -f -` invocations, got %d", len(applyStdins))
	}
	if applyStdins[0] != applyStdins[1] {
		t.Errorf("ruleset diverged between calls:\nfirst:\n%s\nsecond:\n%s", applyStdins[0], applyStdins[1])
	}
	if !strings.Contains(applyStdins[0], "table ip "+nftTableName) {
		t.Errorf("apply stdin missing table header for %q:\n%s", nftTableName, applyStdins[0])
	}
}

// ensureVethForwarding writes 1 to /proc/sys/net/ipv4/conf/<dev>/forwarding
// for both veth ends. On a test process the veth interfaces are not created,
// so the sysctl files do not exist and os.WriteFile fails with ENOENT.
// Verify that the error is surfaced (wrapped, naming the device) rather than
// silently swallowed — silent degradation here would mean live nodes serve
// DNAT traffic with forwarding off, which black-holes return packets.
func TestEnsureVethForwardingSurfacesSysctlError(t *testing.T) {
	// Use a veth name that is guaranteed not to be a real interface so
	// /proc/sys/net/ipv4/conf/<dev>/forwarding does not exist. We can't
	// override the constant vethDefaultName from the test, so we rely on
	// the agent never being run in a sandbox where it actually exists.
	if _, err := os.Stat("/proc/sys/net/ipv4/conf/" + vethDefaultName + "/forwarding"); err == nil {
		t.Skipf("veth %q exists in this test environment; cannot exercise the sysctl-failure path", vethDefaultName)
	}

	t.Run("base_forwarding_only", func(t *testing.T) {
		rm := &RouteManager{}
		err := rm.ensureVethForwarding()
		if err == nil {
			t.Fatal("expected error when veth sysctl path is missing, got nil")
		}
		if !strings.Contains(err.Error(), "enable forwarding on "+vethDefaultName) {
			t.Errorf("error %q does not name the failing device wrapper", err)
		}
	})

	t.Run("port_forward_accept_local", func(t *testing.T) {
		rm := &RouteManager{cfg: Config{PortForwardEnabled: true}}
		err := rm.ensureVethForwarding()
		if err == nil {
			t.Fatal("expected error when veth sysctl path is missing, got nil")
		}
		// The base forwarding loop fails first, so we never reach the
		// accept_local branch — but the test still pins the contract that
		// errors propagate before any "best effort" silent skip.
		if !strings.Contains(err.Error(), "forwarding") {
			t.Errorf("error %q does not reference the forwarding sysctl path", err)
		}
	})
}

func atoiOrFail(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("non-numeric filename suffix %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}
