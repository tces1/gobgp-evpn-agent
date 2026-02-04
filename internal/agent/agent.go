package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"time"

	api "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/apiutil"
	apibgp "github.com/osrg/gobgp/v3/pkg/packet/bgp"
	"github.com/vishvananda/netlink"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"log/slog"

	"gobgp-evpn-agent/internal/config"
	"gobgp-evpn-agent/internal/netutil"
	"gobgp-evpn-agent/internal/vxlan"
)

type Agent struct {
	cfg            config.Config
	localIP        net.IP
	communityToVNI map[uint32]config.VNIConfig
	idToVNI        map[uint32]config.VNIConfig
	vxlanManagers  map[uint32]*vxlan.Manager
	// dynamicVNI means VNI mapping is derived from local vxlan devices.
	dynamicVNI  bool
	mapMu       sync.Mutex
	vniOnline   map[uint32]bool
	mu          sync.Mutex
	desiredMu   sync.Mutex
	desired     map[uint32]map[string]struct{}
	localPathMu sync.Mutex
	localPath   *api.Path
	localComms  []uint32
	conn        *grpc.ClientConn
	client      api.GobgpApiClient
}

// New constructs the agent and prepares static state.
func New(cfg config.Config) (*Agent, error) {
	localIP := net.ParseIP(cfg.Node.LocalAddress)
	if localIP == nil {
		var err error
		localIP, err = netutil.IPv4ForInterface(cfg.Node.LocalInterface)
		if err != nil {
			return nil, err
		}
	}
	if localIP.To4() == nil {
		return nil, fmt.Errorf("local IP must be IPv4")
	}

	communityToVNI := make(map[uint32]config.VNIConfig, len(cfg.VNIs))
	idToVNI := make(map[uint32]config.VNIConfig, len(cfg.VNIs))
	vxManagers := make(map[uint32]*vxlan.Manager, len(cfg.VNIs))
	dynamicVNI := len(cfg.VNIs) == 0
	if !dynamicVNI {
		for _, v := range cfg.VNIs {
			val, err := config.ParseCommunity(v.Community)
			if err != nil {
				return nil, err
			}
			if _, exists := communityToVNI[val]; exists {
				return nil, fmt.Errorf("duplicate community %s across VNIs", v.Community)
			}
			communityToVNI[val] = v
			idToVNI[v.ID] = v
			vxManagers[v.ID] = vxlan.NewManager(v, cfg.Node.VXLANPort, localIP)
		}
	}

	a := &Agent{
		cfg:            cfg,
		localIP:        localIP,
		communityToVNI: communityToVNI,
		idToVNI:        idToVNI,
		vxlanManagers:  vxManagers,
		dynamicVNI:     dynamicVNI,
		vniOnline:      make(map[uint32]bool, len(vxManagers)),
		desired:        make(map[uint32]map[string]struct{}),
	}
	if err := a.connect(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Agent) connect() error {
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.GoBGP.Timeout)
	defer cancel()
	conn, err := grpc.DialContext(
		ctx,
		a.cfg.GoBGP.Address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                2 * time.Minute,
			Timeout:             20 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return fmt.Errorf("connect gobgp at %s: %w", a.cfg.GoBGP.Address, err)
	}
	a.conn = conn
	a.client = api.NewGobgpApiClient(conn)
	return nil
}

// Run blocks until context cancellation.
func (a *Agent) Run(ctx context.Context) error {
	if a.dynamicVNI {
		// Seed VNI map from existing vxlan links.
		a.refreshDynamicVNIs(ctx)
	}
	// Initial probe: do not create vxlan, only update online state.
	for vni := range a.vxlanManagers {
		_ = a.ensureVNI(ctx, vni)
	}
	// Periodic probe: detect manual vxlan create/delete at runtime.
	go a.pollVxlan(ctx, 2*time.Second)
	if a.cfg.AdvertiseSelf {
		if err := a.advertiseSelf(ctx); err != nil {
			return fmt.Errorf("announce self: %w", err)
		}
	}

	for {
		if err := a.watchOnce(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			slog.Warn("watch stream ended, retrying", "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
			continue
		}
		return nil
	}
}

// Close releases resources.
func (a *Agent) Close() {
	if a.conn != nil {
		_ = a.conn.Close()
	}
	if a.cfg.Node.SkipLinkCleanup {
		return
	}
	for _, mgr := range a.vxlanManagers {
		_ = mgr.Close()
	}
}

func (a *Agent) advertiseSelf(ctx context.Context) error {
	for _, v := range a.cfg.VNIs {
		if mgr := a.vxlanManagers[v.ID]; mgr != nil {
			if err := mgr.LoadLink(); err != nil && !a.cfg.Node.AutoRecreateVxlan {
				slog.Info("skip advertise, vxlan missing", "vni", v.ID, "dev", v.Device)
				a.setOnline(v.ID, false)
				continue
			}
		}
		a.setOnline(v.ID, true)
	}
	return a.updateLocalPath(ctx)
}

func (a *Agent) watchOnce(ctx context.Context) error {
	stream, err := a.client.WatchEvent(ctx, &api.WatchEventRequest{
		Table: &api.WatchEventRequest_Table{
			Filters: []*api.WatchEventRequest_Table_Filter{
				{
					Type: api.WatchEventRequest_Table_Filter_BEST,
					Init: true,
				},
			},
		},
		BatchSize: 128,
	})
	if err != nil {
		return fmt.Errorf("start watch: %w", err)
	}

	for {
		resp, err := stream.Recv()
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return fmt.Errorf("watch recv: %w", err)
		}
		table := resp.GetTable()
		if table == nil {
			continue
		}
		a.desiredMu.Lock()
		touched := a.consumePaths(table.Paths, a.desired)
		a.desiredMu.Unlock()
		for vni := range touched {
			if mgr := a.vxlanManagers[vni]; mgr != nil {
				if !a.ensureVNI(ctx, vni) {
					continue
				}
				if err := mgr.SyncFDB(a.snapshotDesired(vni)); err != nil {
					if !errors.Is(err, netlink.LinkNotFoundError{}) {
						slog.Error("sync fdb failed", "vni", vni, "err", err)
					}
				}
			}
		}
	}
}

func (a *Agent) consumePaths(paths []*api.Path, desired map[uint32]map[string]struct{}) map[uint32]struct{} {
	touched := make(map[uint32]struct{})
	for _, p := range paths {
		if p == nil || p.Family == nil {
			continue
		}
		if p.Family.Afi != api.Family_AFI_IP || p.Family.Safi != api.Family_SAFI_UNICAST {
			continue
		}
		nlri, err := apiutil.GetNativeNlri(p)
		if err != nil {
			slog.Debug("skip path with bad nlri", "err", err)
			continue
		}
		ipPrefix, ok := nlri.(*apibgp.IPAddrPrefix)
		if !ok || ipPrefix.Length != 32 {
			continue
		}
		ip := ipPrefix.Prefix.String()
		if ip == a.localIP.String() {
			continue
		}
		comms, err := extractCommunities(p)
		if err != nil {
			slog.Debug("skip path, cannot extract communities", "err", err)
			continue
		}
		for _, comm := range comms {
			a.mapMu.Lock()
			vniCfg, ok := a.communityToVNI[comm]
			a.mapMu.Unlock()
			if !ok {
				continue
			}
			if desired[vniCfg.ID] == nil {
				desired[vniCfg.ID] = make(map[string]struct{})
			}
			if p.IsWithdraw {
				delete(desired[vniCfg.ID], ip)
			} else {
				desired[vniCfg.ID][ip] = struct{}{}
			}
			touched[vniCfg.ID] = struct{}{}
		}
	}
	return touched
}

// ensureVNI ensures vxlan link exists if allowed; returns false if VNI is offline.
func (a *Agent) ensureVNI(ctx context.Context, vni uint32) bool {
	a.mapMu.Lock()
	mgr := a.vxlanManagers[vni]
	a.mapMu.Unlock()
	if mgr == nil {
		return false
	}
	if err := mgr.LoadLink(); err == nil {
		if online, _ := a.getOnline(vni); !online {
			a.mapMu.Lock()
			vCfg, ok := a.idToVNI[vni]
			a.mapMu.Unlock()
			if ok {
				slog.Info("vxlan detected", "vni", vni, "dev", vCfg.Device)
			}
			a.setOnline(vni, true)
			_ = a.updateLocalPath(ctx)
		}
		return true
	}
	// Link missing: withdraw membership and mark offline.
	if online, _ := a.getOnline(vni); online {
		a.mapMu.Lock()
		vCfg, ok := a.idToVNI[vni]
		a.mapMu.Unlock()
		if ok {
			slog.Info("vxlan removed", "vni", vni, "dev", vCfg.Device)
		}
		a.setOnline(vni, false)
		_ = a.updateLocalPath(ctx)
	}
	return false
}

func (a *Agent) pollVxlan(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.dynamicVNI {
				a.refreshDynamicVNIs(ctx)
			}
			for vni := range a.vxlanManagers {
				if !a.ensureVNI(ctx, vni) {
					continue
				}
				if mgr := a.vxlanManagers[vni]; mgr != nil {
					if err := mgr.SyncFDB(a.snapshotDesired(vni)); err != nil {
						if !errors.Is(err, netlink.LinkNotFoundError{}) {
							slog.Error("sync fdb failed", "vni", vni, "err", err)
						}
					}
				}
			}
		}
	}
}

func (a *Agent) refreshDynamicVNIs(ctx context.Context) {
	links, err := netlink.LinkList()
	if err != nil {
		slog.Warn("list links failed", "err", err)
		return
	}
	// Track current vxlan VNIs on the host.
	present := make(map[uint32]struct{})
	created := false
	for _, l := range links {
		vx, ok := l.(*netlink.Vxlan)
		if !ok {
			continue
		}
		vni := uint32(vx.VxlanId)
		if vni == 0 {
			continue
		}
		present[vni] = struct{}{}
		// Derive community from ASN:VNI convention.
		community := fmt.Sprintf("%d:%d", a.cfg.CommunityASN, vni)
		commVal, err := config.ParseCommunity(community)
		if err != nil {
			slog.Warn("invalid community for vni", "vni", vni, "community", community, "err", err)
			continue
		}
		a.mapMu.Lock()
		if _, exists := a.idToVNI[vni]; exists {
			a.mapMu.Unlock()
			continue
		}
		vniCfg := config.VNIConfig{
			ID:                vni,
			Community:         community,
			Device:            l.Attrs().Name,
			UnderlayInterface: a.cfg.Node.LocalInterface,
		}
		a.idToVNI[vni] = vniCfg
		a.communityToVNI[commVal] = vniCfg
		a.vxlanManagers[vni] = vxlan.NewManager(vniCfg, a.cfg.Node.VXLANPort, a.localIP)
		a.mapMu.Unlock()
		slog.Info("discovered vxlan vni", "vni", vni, "dev", l.Attrs().Name, "community", community)
		created = true
	}
	if created {
		// New VNI appeared; rebuild desired table from RIB and sync FDB.
		touched := a.resyncRIB(ctx)
		for vni := range touched {
			if mgr := a.vxlanManagers[vni]; mgr != nil {
				if !a.ensureVNI(ctx, vni) {
					continue
				}
				if err := mgr.SyncFDB(a.snapshotDesired(vni)); err != nil {
					if !errors.Is(err, netlink.LinkNotFoundError{}) {
						slog.Error("sync fdb failed", "vni", vni, "err", err)
					}
				}
			}
		}
	}
	// Remove VNIs that no longer exist on the host.
	var missing []uint32
	a.mapMu.Lock()
	for vni := range a.vxlanManagers {
		if _, ok := present[vni]; !ok {
			missing = append(missing, vni)
		}
	}
	a.mapMu.Unlock()
	for _, vni := range missing {
		_ = a.ensureVNI(ctx, vni) // withdraw if needed
		var dev string
		a.mapMu.Lock()
		if cfg, ok := a.idToVNI[vni]; ok {
			dev = cfg.Device
		}
		delete(a.idToVNI, vni)
		for comm, cfg := range a.communityToVNI {
			if cfg.ID == vni {
				delete(a.communityToVNI, comm)
				break
			}
		}
		delete(a.vxlanManagers, vni)
		a.mapMu.Unlock()
		a.desiredMu.Lock()
		delete(a.desired, vni)
		a.desiredMu.Unlock()
		slog.Info("unregistered vxlan vni", "vni", vni, "dev", dev)
	}
}

func (a *Agent) resyncRIB(ctx context.Context) map[uint32]struct{} {
	touched := make(map[uint32]struct{})
	// Pull a full snapshot when VNIs appear to avoid missing FDB entries.
	ctx, cancel := context.WithTimeout(ctx, a.cfg.GoBGP.Timeout)
	defer cancel()
	stream, err := a.client.ListPath(ctx, &api.ListPathRequest{
		TableType: api.TableType_GLOBAL,
		Family:    &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST},
	})
	if err != nil {
		slog.Warn("list path failed", "err", err)
		return touched
	}
	a.desiredMu.Lock()
	a.desired = make(map[uint32]map[string]struct{})
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			slog.Warn("list path recv failed", "err", err)
			break
		}
		dest := resp.GetDestination()
		if dest == nil || len(dest.Paths) == 0 {
			continue
		}
		for vni := range a.consumePaths(dest.Paths, a.desired) {
			touched[vni] = struct{}{}
		}
	}
	a.desiredMu.Unlock()
	return touched
}

func (a *Agent) getOnline(vni uint32) (bool, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	val, ok := a.vniOnline[vni]
	return val, ok
}

func (a *Agent) setOnline(vni uint32, online bool) {
	a.mu.Lock()
	a.vniOnline[vni] = online
	a.mu.Unlock()
}

func (a *Agent) snapshotDesired(vni uint32) map[string]struct{} {
	a.desiredMu.Lock()
	defer a.desiredMu.Unlock()
	src := a.desired[vni]
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]struct{}, len(src))
	for k := range src {
		dst[k] = struct{}{}
	}
	return dst
}

func extractCommunities(p *api.Path) ([]uint32, error) {
	attrs, err := apiutil.GetNativePathAttributes(p)
	if err != nil {
		return nil, err
	}
	var res []uint32
	for _, attr := range attrs {
		if comm, ok := attr.(*apibgp.PathAttributeCommunities); ok {
			res = append(res, comm.Value...)
		}
	}
	return res, nil
}

func newCommunityPath(prefix string, communities []uint32) (*api.Path, error) {
	nlri := apibgp.NewIPAddrPrefix(32, prefix)
	attrs := []apibgp.PathAttributeInterface{
		apibgp.NewPathAttributeOrigin(0),
		apibgp.NewPathAttributeNextHop(prefix),
	}
	if len(communities) > 0 {
		attrs = append(attrs, apibgp.NewPathAttributeCommunities(communities))
	}
	return apiutil.NewPath(nlri, false, attrs, time.Now())
}

func (a *Agent) updateLocalPath(ctx context.Context) error {
	// Publish a single local /32 path carrying all active VNI communities.
	comms := a.collectLocalCommunities()
	a.localPathMu.Lock()
	if equalComms(a.localComms, comms) {
		a.localPathMu.Unlock()
		return nil
	}
	oldPath := a.localPath
	a.localComms = append([]uint32(nil), comms...)
	a.localPathMu.Unlock()

	if oldPath != nil {
		_, _ = a.client.DeletePath(ctx, &api.DeletePathRequest{
			TableType: api.TableType_GLOBAL,
			Path:      oldPath,
		})
	}

	if len(comms) == 0 {
		a.localPathMu.Lock()
		a.localPath = nil
		a.localPathMu.Unlock()
		return nil
	}

	path, err := newCommunityPath(a.localIP.String(), comms)
	if err != nil {
		return err
	}
	if _, err := a.client.AddPath(ctx, &api.AddPathRequest{TableType: api.TableType_GLOBAL, Path: path}); err != nil {
		return fmt.Errorf("add path for local membership: %w", err)
	}
	a.localPathMu.Lock()
	a.localPath = path
	a.localPathMu.Unlock()
	slog.Info("advertised membership", "prefix", a.localIP.String()+"/32", "communities", comms)
	return nil
}

func (a *Agent) collectLocalCommunities() []uint32 {
	a.mapMu.Lock()
	defer a.mapMu.Unlock()
	var comms []uint32
	for vni, online := range a.vniOnline {
		if !online {
			continue
		}
		if cfg, ok := a.idToVNI[vni]; ok {
			if comm, err := config.ParseCommunity(cfg.Community); err == nil {
				comms = append(comms, comm)
			}
		}
	}
	sort.Slice(comms, func(i, j int) bool { return comms[i] < comms[j] })
	return comms
}

func equalComms(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
