package p2p

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/astaxie/beego/logs"
	"github.com/aucusaga/gohotstuff/libs"
	ipfsaddr "github.com/ipfs/go-ipfs-addr"
	"github.com/libp2p/go-libp2p"
	circuit "github.com/libp2p/go-libp2p-circuit"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	secio "github.com/libp2p/go-libp2p-secio"
	"github.com/multiformats/go-multiaddr"
)

const (
	protocolPrefix = "/gohotstuff/p2p"
)

var (
	defaultTickerTimeSec = 4
)

type Switch struct {
	cfg *Config

	id    *multiaddr.Multiaddr
	host  host.Host
	kdht  *dht.IpfsDHT
	peers *PeerSet
	timer *time.Ticker
	quit  chan struct{}

	reactor map[Module]libs.Reactor
	mtx     sync.Mutex

	log libs.Logger
}

func NewSwitch(cfg *Config, logger libs.Logger) (*Switch, error) {
	if cfg.TickerTimeSec == 0 {
		cfg.TickerTimeSec = int64(defaultTickerTimeSec)
	}
	if logger == nil {
		logger = logs.NewLogger()
	}
	sw := &Switch{
		quit:    make(chan struct{}),
		cfg:     cfg,
		peers:   NewPeerSet(),
		timer:   time.NewTicker(time.Duration(cfg.TickerTimeSec) * time.Second),
		reactor: make(map[Module]libs.Reactor),
		log:     logger,
	}

	sw.log.Info("new a switch succ, cfg: %+v", cfg)
	return sw, nil
}

// AddReactor should be invoked before switch.Start(),
// consensus module must be registered.
func (sw *Switch) AddReactor(mo Module, f libs.Reactor) error {
	sw.mtx.Lock()
	defer sw.mtx.Unlock()

	if _, ok := sw.reactor[mo]; ok {
		return fmt.Errorf("module has been registered before, %v", mo)
	}

	sw.reactor[mo] = f
	sw.log.Info("[module %s] registered", mo)
	return nil
}

func (sw *Switch) Start() error {
	privData, err := base64.StdEncoding.DecodeString(string(sw.cfg.PrivateKey))
	if err != nil {
		return err
	}
	priv, err := crypto.UnmarshalPrivateKey(privData)
	if err != nil {
		return err
	}
	opts := []libp2p.Option{
		libp2p.ListenAddrStrings(sw.cfg.Address),
		libp2p.EnableRelay(circuit.OptHop),
		libp2p.Identity(priv),
		libp2p.Security(secio.ID, secio.New),
	}
	ctx := context.Background()
	host, err := libp2p.New(ctx, opts...)
	if err != nil {
		sw.log.Error("new libp2p host failed @ p2p.Start, err: %v", err)
		return err
	}

	sw.host = host
	sw.host.SetStreamHandler(protocol.ID(protocolPrefix), sw.handleStream)
	// Build host multiaddress
	hostAddr, _ := multiaddr.NewMultiaddr(fmt.Sprintf("/p2p/%s", sw.host.ID().Pretty()))
	addr := sw.host.Addrs()[0]
	fullAddr := addr.Encapsulate(hostAddr)
	sw.id = &fullAddr
	sw.log.Info("new p2pnode @ p2p.Start host's multiaddr: %s", fullAddr)

	dhtOpts := []dht.Option{
		dht.Mode(dht.ModeServer),
		dht.RoutingTableRefreshPeriod(3 * time.Second),
		dht.ProtocolPrefix(protocol.ID(protocolPrefix)),
	}
	if sw.kdht, err = dht.New(ctx, host, dhtOpts...); err != nil {
		sw.log.Error("new dht host failed @ p2p.Start, err: %v", err)
		return err
	}

	if err := sw.bootstrap(ctx); err != nil {
		sw.log.Error("bootstrap failed @ p2p.Start, err: %v", err)
		return err
	}

	go sw.acceptRoutine()

	return nil
}

func (sw *Switch) Stop() error {
	defer sw.timer.Stop()

	f := func(peer Peer) bool {
		peer.FlushStop()
		return true
	}
	rchan := sw.peers.Range(f)
	<-rchan

	sw.quit <- struct{}{}
	return nil
}

// Peers

// Broadcast runs a go routine for each attempted send, which will block trying
// to send for defaultSendTimeoutSeconds. Returns a channel which receives
// success values for each attempted send (false if times out). Channel will be
// closed once msg bytes are sent to all peers (or time out).
//
// NOTE: Broadcast uses goroutines, so order of broadcast may not be preserved.
func (sw *Switch) Broadcast(chID int32, msgBytes []byte) {
	f := func(p Peer) bool {
		return p.Send(chID, msgBytes)
	}
	ch := sw.peers.Range(f)
	<-ch

	sw.log.Info("Broadcast completed @ Broadcast, chID: %d, msgBytes: %X", chID, msgBytes)
}

func (sw *Switch) Send(pr string, chID int32, msgBytes []byte) error {
	id, err := peer.Decode(pr)
	if err != nil {
		sw.log.Error("fail to convert string to id @ Send, err: %v, peer_id: %v, chID: %d, msgBytes: %X", err, pr, chID, msgBytes)
	}
	p, err := sw.peers.Find(id)
	if err != nil {
		sw.log.Error("fail to find peer @ Send, err: %v, peer_id: %v, chID: %d, msgBytes: %X", err, pr, chID, msgBytes)
		return err
	}
	if !p.Send(chID, msgBytes) {
		return fmt.Errorf("fail to send @ Send, err: %v, peer_id: %v, chID: %d, msgBytes: %X", err, pr, chID, msgBytes)
	}
	return nil
}

func (sw *Switch) GetP2PID(peerID string) (string, error) {
	return peerID, nil
}

func (sw *Switch) genPeerMultiID(id peer.ID) string {
	peer := sw.host.Peerstore().PeerInfo(id)
	if len(peer.Addrs) == 0 {
		return ""
	}
	return fmt.Sprintf("%s/p2p/%s", peer.Addrs[0], peer.ID)
}

func (sw *Switch) bootstrap(ctx context.Context) error {
	if err := sw.kdht.Bootstrap(ctx); err != nil {
		sw.log.Error("kdht bootstrap failed @ p2p.bootstrap, err: %v", err)
		return err
	}

retry:
	for _, peerMutltiID := range sw.cfg.BootStrap {
		if err := sw.connect(peerMutltiID); err != nil {
			continue
		}
		sw.log.Info("build stream success @ bootstrap remote_peer: %s", peerMutltiID)
	}

	if len(sw.kdht.RoutingTable().ListPeers()) == 0 {
		goto retry
	}

	return nil
}

// DialPeersAsync dials a list of peers asynchronously in random order.
// Used to dial peers from config on startup or from unsafe-RPC (trusted sources).
// It ignores ErrNetAddressLookup. However, if there are other errors, first
// encounter is returned.
// Nop if there are no peers.
func (sw *Switch) dialPeersAsync(id peer.ID) error {
	old, err := sw.peers.Find(id)
	if err == nil {
		if err := old.Validate(); err == nil {
			return nil
		}
	}
	ctx := context.Background()
	stream, err := sw.host.NewStream(ctx, id, protocol.ID(protocolPrefix))
	if err != nil {
		sw.log.Error("host make newstream fail @ DialPeersAsync, peer_id: %s, err: %v", id.Pretty(), err)
		return err
	}
	rawPeer := sw.host.Peerstore().PeerInfo(id)
	peer, err := NewDefaultPeer(&rawPeer, stream, sw.reactor, sw.log)
	if err != nil {
		sw.log.Error("new remote peer fail @ DialPeersAsync, peer_id: %s, err: %v", id.Pretty(), err)
		stream.Close()
		sw.kdht.RoutingTable().RemovePeer(id)
		return err
	}
	sw.peers.Add(peer)
	peer.Start()
	return nil
}

func (sw *Switch) acceptRoutine() {
	for {
		select {
		case <-sw.timer.C:
			for _, peerID := range sw.kdht.RoutingTable().ListPeers() {
				if _, err := sw.peers.Find(peerID); err == nil {
					continue
				}
				multiAddr := sw.genPeerMultiID(peerID)
				if multiAddr == "" {
					continue
				}
				if err := sw.connect(multiAddr); err != nil {
					continue
				}
				sw.log.Info("connect peer from router table @ p2p.acceptRoutine, peer_id: %s", peerID.Pretty())
			}
		case <-sw.quit:
			sw.log.Error("switch meets end @ p2p.acceptRoutine, return")
			return
		}
	}
}

func (sw *Switch) connect(multiAddr string) error {
	peerAddr, err := ipfsaddr.ParseString(multiAddr)
	if err != nil {
		sw.log.Error("parse string failed @ p2p.acceptRoutine, multi_peer: %s, err: %v", multiAddr, err)
		return err
	}
	addrInfo, err := peer.AddrInfoFromP2pAddr(peerAddr.Multiaddr())
	if err != nil {
		sw.log.Error("add addrinfo failed @ p2p.acceptRoutine, multi_peer: %s, err: %v", multiAddr, err)
		return err
	}
	if err := sw.host.Connect(context.Background(), *addrInfo); err != nil {
		sw.log.Error("host connect failed @ p2p.acceptRoutine, peer_id: %s, err: %v", addrInfo.ID.Pretty(), err)
		return err
	}
	if err := sw.dialPeersAsync(addrInfo.ID); err != nil {
		sw.log.Error("dial fail @ p2p.acceptRoutine peer_id: %s, err: %v", addrInfo.ID.Pretty(), err)
	}
	return nil
}

func (sw *Switch) handleStream(netStream network.Stream) {
	old, err := sw.peers.Find(netStream.Conn().RemotePeer())
	if err == nil {
		if err := old.Validate(); err == nil {
			sw.log.Error("use an old one @ handleStream, peer_id: %s", netStream.Conn().RemotePeer())
			return
		}
	}
	p := sw.host.Peerstore().PeerInfo(netStream.Conn().RemotePeer())
	peer, err := NewDefaultPeer(&p, netStream, sw.reactor, sw.log)
	if err != nil {
		sw.log.Error("new remote peer fail @ handleStream, peer_id: %s, err: %v", netStream.Conn().RemotePeer(), err)
		return
	}
	sw.peers.Add(peer)
	peer.Start()
	sw.log.Info("build stream success from a new remote peer @ handleStream, peer_id: %s", netStream.Conn().RemotePeer())
}

// ---------------------------------------------------------------------------------------------------
type Config struct {
	Address    string
	BootStrap  []string
	PrivateKey string // only for networking
	PublicKey  string // only for networking

	TickerTimeSec int64
}
