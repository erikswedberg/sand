package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/banksean/sand/internal/hostops"
	"github.com/banksean/sand/internal/sandtypes"
)

// setupHostPorts, for each host-loopback port requested for this sandbox,
// starts a daemon-side TCP forwarder on the sandbox's bridge gateway IP and
// then attempts (best-effort) to install an iptables DNAT rule inside the
// sandbox redirecting 127.0.0.1:<port> to <gateway>:<port>. A `host.sand`
// /etc/hosts entry is always added so reaching the service works regardless
// of iptables availability.
//
// We use ContainerService.Exec directly rather than the container hook
// abstraction so we can request uid 0. Apple's container runtime typically
// does not grant CAP_NET_ADMIN, so the iptables step will often fail even
// as root; that is logged but not fatal.
func (s *Service) setupHostPorts(ctx context.Context, sb *sandtypes.Box) error {
	if len(sb.HostPorts) == 0 || s.HostPortManager == nil {
		return nil
	}
	ctr, err := s.Store.GetContainer(ctx, sb.ContainerID)
	if err != nil {
		return fmt.Errorf("host port setup: %w", err)
	}
	if ctr == nil || len(ctr.Networks) == 0 {
		return fmt.Errorf("host port setup: container has no network info")
	}
	gateway := ctr.Networks[0].IPv4Gateway
	if gateway == "" {
		return fmt.Errorf("host port setup: container has no IPv4 gateway")
	}
	if err := s.HostPortManager.StartForSandbox(sb.ID, gateway, sb.HostPorts); err != nil {
		return fmt.Errorf("host port setup: %w", err)
	}

	// Always add /etc/hosts entry — this is the reliable fallback path.
	// Exec as uid 0 directly: sand no longer execs as root by default, and
	// daemon-side execs don't carry supplementary groups (so doas/wheel
	// membership checks fail even for users listed in /etc/group).
	rootExec := &hostops.ExecContainer{ProcessOptions: hostops.ProcessOptions{User: "0:0"}}
	if _, err := s.ContainerService.Exec(ctx,
		rootExec,
		sb.ContainerID, "sh", os.Environ(), "-c",
		buildHostSandEtcHostsScript(gateway),
	); err != nil {
		slog.WarnContext(ctx, "setupHostPorts: failed to update /etc/hosts", "sandbox", sb.ID, "error", err)
	}

	// Best-effort iptables DNAT so 127.0.0.1:<port> resolves transparently.
	// Apple's container runtime typically does not grant CAP_NET_ADMIN to
	// containers, in which case this fails and we silently use the
	// host.sand hostname path instead. The forwarder also HTTP-rewrites the
	// Host header, so most clients work via host.sand:<port> without any
	// further configuration.
	script := buildHostPortIptablesScript(gateway, sb.HostPorts)
	out, execErr := s.ContainerService.Exec(ctx,
		rootExec,
		sb.ContainerID, "sh", os.Environ(), "-c", script)
	if execErr != nil {
		slog.InfoContext(ctx, "setupHostPorts: in-sandbox iptables unavailable; using host.sand fallback",
			"sandbox", sb.ID, "error", execErr, "output", strings.TrimSpace(out))
	} else {
		slog.InfoContext(ctx, "setupHostPorts: iptables installed", "sandbox", sb.ID, "gateway", gateway, "ports", sb.HostPorts)
	}
	fmt.Printf("[sand] host services exposed at http://host.sand:<port>/ (ports: %v)\n", sb.HostPorts)
	return nil
}

// buildHostSandEtcHostsScript returns a shell snippet that, idempotently,
// inserts/refreshes a `<gateway>\thost.sand` line in /etc/hosts.
func buildHostSandEtcHostsScript(gatewayIP string) string {
	return "sed -i.bak '/[[:space:]]host\\.sand$/d' /etc/hosts 2>/dev/null || true; " +
		"printf '%s\\thost.sand\\n' " + gatewayIP + " >> /etc/hosts"
}

func buildHostPortIptablesScript(gatewayIP string, ports []int) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	// Enable redirecting loopback-destined packets via DNAT.
	b.WriteString("sysctl -w net.ipv4.conf.all.route_localnet=1 >/dev/null\n")
	b.WriteString("sysctl -w net.ipv4.conf.lo.route_localnet=1 >/dev/null\n")
	for _, p := range ports {
		ps := strconv.Itoa(p)
		// Output chain handles locally-generated traffic to 127.0.0.1:<port>.
		b.WriteString("iptables -t nat -C OUTPUT -p tcp -d 127.0.0.1 --dport " + ps +
			" -j DNAT --to-destination " + gatewayIP + ":" + ps +
			" 2>/dev/null || iptables -t nat -A OUTPUT -p tcp -d 127.0.0.1 --dport " + ps +
			" -j DNAT --to-destination " + gatewayIP + ":" + ps + "\n")
		// SNAT the return path so connections sourced from loopback get the
		// correct source IP when reaching the host.
		b.WriteString("iptables -t nat -C POSTROUTING -p tcp -d " + gatewayIP +
			" --dport " + ps + " -j MASQUERADE" +
			" 2>/dev/null || iptables -t nat -A POSTROUTING -p tcp -d " + gatewayIP +
			" --dport " + ps + " -j MASQUERADE\n")
	}
	return b.String()
}
