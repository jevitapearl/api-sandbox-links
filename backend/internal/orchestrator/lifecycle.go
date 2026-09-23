package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

// Resource limits applied to every sandbox container. Kept modest so several
// sandboxes can run on a dev machine, and generous enough for typical APIs.
const (
	defaultMemoryLimit = 512 * 1024 * 1024 // 512 MB
	defaultCPUQuota    = 0.75              // 75% of one core
	defaultPIDsLimit   = 256
	imageUser          = "" // stay as image default (often root); gVisor is future hardening
)

var defaultPIDs = int64(defaultPIDsLimit)

// RunOptions describes a container the orchestrator should start for a sandbox.
type RunOptions struct {
	SandboxID   string
	ImageTag    string
	Subdomain   string
	InternalPort int
	Env         []string        // KEY=VALUE pairs, already decrypted
	NetworkName string          // private per-sandbox network
	Labels      map[string]string // extra labels (traefik, provenance)
	User        string
}

// RunResult reports where the container landed so callers can store it.
type RunResult struct {
	ContainerID string
	IPAddress   string
	Port        int
}

// netName returns the private Docker network name for a sandbox. It is
// predictable from the sandbox ID so tear-down and forking can reuse it.
func netName(sandboxID string) string { return "sandbox_" + sandboxID[:8] }

// volumeName returns a Docker volume name for a sandbox (used by sidecar DBs).
func volumeName(sandboxID string) string { return "sandboxdata_" + sandboxID[:8] }

// ensureNetwork creates the sandbox's private network if it is not present.
// One private network per sandbox is what gives the platform its isolation
// story: the app container and its sidecar DB can talk to each other but no
// other tenant's containers can.
func (o *Orchestrator) ensureNetwork(ctx context.Context, sandboxID string) (string, error) {
	name := netName(sandboxID)
	_, err := o.docker.NetworkInspect(ctx, name, network.InspectOptions{})
	if err == nil {
		return name, nil
	}
	if !client.IsErrNotFound(err) {
		return "", fmt.Errorf("orchestrator: inspecting network %s: %w", name, err)
	}
	r, err := o.docker.NetworkCreate(ctx, name, network.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{"api-sandbox-links.purpose": "private-sandbox"},
	})
	if err != nil {
		return "", fmt.Errorf("orchestrator: creating network %s: %w", name, err)
	}
	return r.ID, nil
}

// RunSandbox provisions the private network and starts the app container with
// resource limits and Traefik labels. It is a single synchronous step used by
// the deploy pipeline and the wake middleware.
//
// DECISION (routing): sandbox traffic is edge-routed by a Traefik wildcard
// rule into the backend's gateway (see internal/proxy/wake_middleware.go) so
// hibernated sandboxes can be woken mid-request. Each container still carries
// complete per-subdomain Traefik labels at low priority so direct router
// metadata exists (and would take over if wake-on-request were ever removed);
// the wildcard router wins in normal operation.
func (o *Orchestrator) RunSandbox(ctx context.Context, opts RunOptions) (*RunResult, error) {
	netID, err := o.ensureNetwork(ctx, opts.SandboxID)
	if err != nil {
		return nil, err
	}
	_ = netID

	labels := map[string]string{
		"api-sandbox-links.sandbox-id": opts.SandboxID,
		"api-sandbox-links.subdomain":  opts.Subdomain,
		// Traefik direct-route fallback (priority 1 loses to the wildcard
		// gateway router — see DECISION above).
		"traefik.enable": "true",
		"traefik.http.routers." + opts.Subdomain + ".rule":       "Host(`" + opts.Subdomain + "." + o.cfg.SandboxBaseDomain + "`)",
		"traefik.http.routers." + opts.Subdomain + ".entrypoints": "web",
		"traefik.http.routers." + opts.Subdomain + ".priority":    "1",
		"traefik.http.routers." + opts.Subdomain + ".service":     opts.Subdomain,
		"traefik.http.services." + opts.Subdomain + ".loadbalancer.server.port": itoa(opts.InternalPort),
	}
	for k, v := range opts.Labels {
		labels[k] = v
	}

	cc := &container.Config{
		Image: opts.ImageTag,
		Env:   opts.Env,
		Labels: labels,
	}
	if opts.User != "" {
		cc.User = opts.User
	}
	hc := &container.HostConfig{
		Resources: container.Resources{
			// Promoted fields (embedded struct) must be set via the Resources
			// key inside a composite literal.
			Memory:    defaultMemoryLimit,
			NanoCPUs:  int64(defaultCPUQuota * 1e9),
			PidsLimit: &defaultPIDs,
		},
		RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
	}
	nc := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			netName(opts.SandboxID): {NetworkID: ""},
		},
	}

	resp, err := o.docker.ContainerCreate(ctx, cc, hc, nc, nil, "sandbox_"+opts.SandboxID[:8]+"_"+opts.Subdomain)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: creating sandbox container: %w", err)
	}
	if err := o.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("orchestrator: starting sandbox container: %w", err)
	}

	ip, err := o.ContainerIP(ctx, resp.ID, netName(opts.SandboxID))
	if err != nil {
		return nil, err
	}
	return &RunResult{ContainerID: resp.ID, IPAddress: ip, Port: opts.InternalPort}, nil
}

// ContainerIP resolves a container's address on the given network.
func (o *Orchestrator) ContainerIP(ctx context.Context, containerID, networkName string) (string, error) {
	info, err := o.docker.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("orchestrator: inspecting container %s: %w", containerID, err)
	}
	ip := info.NetworkSettings.Networks[networkName].IPAddress
	if ip == "" {
		return "", fmt.Errorf("orchestrator: container %s has no address on network %s", containerID, networkName)
	}
	return ip, nil
}

// StartContainer starts an existing (hibernated) container.
func (o *Orchestrator) StartContainer(ctx context.Context, containerID string) error {
	if err := o.docker.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("orchestrator: starting container %s: %w", containerID, err)
	}
	return nil
}

// StopContainer stops a container without removing it (hibernate path).
func (o *Orchestrator) StopContainer(ctx context.Context, containerID string) error {
	timeout := 10
	if err := o.docker.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("orchestrator: stopping container %s: %w", containerID, err)
	}
	return nil
}

// RemoveContainer force-removes a container and its volumes (destroy path).
func (o *Orchestrator) RemoveContainer(ctx context.Context, containerID string) error {
	if err := o.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true, RemoveVolumes: false}); err != nil {
		return fmt.Errorf("orchestrator: removing container %s: %w", containerID, err)
	}
	return nil
}

// RemoveVolume deletes a named Docker volume (destroy path — irreversible).
func (o *Orchestrator) RemoveVolume(ctx context.Context, name string) error {
	if err := o.docker.VolumeRemove(ctx, name, true); err != nil {
		return fmt.Errorf("orchestrator: removing volume %s: %w", name, err)
	}
	return nil
}

// RemoveNetwork deletes a private sandbox network (destroy path).
func (o *Orchestrator) RemoveNetwork(ctx context.Context, sandboxID string) error {
	if err := o.docker.NetworkRemove(ctx, netName(sandboxID)); err != nil {
		return fmt.Errorf("orchestrator: removing network %s: %w", netName(sandboxID), err)
	}
	return nil
}

// WaitHealthy polls the container's port until it accepts a TCP connection or
// the timeout elapses. Used by the wake middleware before forwarding a request
// and by the deploy pipeline before marking a sandbox running.
func (o *Orchestrator) WaitHealthy(ctx context.Context, containerID, ip string, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("orchestrator: waiting for container %s health: %w", containerID, ctx.Err())
		case <-ticker.C:
		}
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, itoa(port)), 2*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("orchestrator: container %s not healthy after %s (last err: %v)", containerID, timeout, err)
		}
	}
}

// discoverPortCandidates are probed (in order) when the nixpacks-detected port
// doesn't match what the app actually binds. Some apps hardcode a port instead
// of honoring PORT, so the planned port can be wrong; the gateway routes to
// whatever the deploy records as the container's internal port.
var discoverPortCandidates = []int{8080, 8000, 3000, 5000, 5001, 4000, 9000, 8888, 80}

// DiscoverPort finds which port the container's app actually listens on. The
// planned (nixpacks-detected) port is tried first, then a set of well-known
// app ports, returning the first one that accepts a TCP connection. App
// startup can be slow (waiting on a database), so probing runs until timeout.
func (o *Orchestrator) DiscoverPort(ctx context.Context, ip string, planned int, timeout time.Duration) (int, error) {
	seen := map[int]bool{}
	candidates := make([]int, 0, len(discoverPortCandidates)+1)
	for _, p := range append([]int{planned}, discoverPortCandidates...) {
		if !seen[p] {
			seen[p] = true
			candidates = append(candidates, p)
		}
	}
	deadline := time.Now().Add(timeout)
	for i := 0; ; i++ {
		if ctx.Err() != nil {
			return 0, fmt.Errorf("orchestrator: discovering app port: %w", ctx.Err())
		}
		port := candidates[i%len(candidates)]
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, itoa(port)), 2*time.Second)
		if err == nil {
			conn.Close()
			o.logger.Info("deploy: discovered app listening port", slog.Int("port", port))
			return port, nil
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("orchestrator: no listening port found on %s within %s (last err: %v)", ip, timeout, err)
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("orchestrator: discovering app port: %w", ctx.Err())
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// FindContainersBySandbox lists every container tagged with the given sandbox
// ID (the app container plus any sidecar DB). Used for full cleanup.
func (o *Orchestrator) FindContainersBySandbox(ctx context.Context, sandboxID string) ([]container.Summary, error) {
	f := filters.NewArgs()
	f.Add("label", "api-sandbox-links.sandbox-id="+sandboxID)
	all, err := o.docker.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, fmt.Errorf("orchestrator: listing sandbox containers: %w", err)
	}
	return all, nil
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}