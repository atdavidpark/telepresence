package tun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"

	"github.com/datawire/dlib/dgroup"
	"github.com/datawire/dlib/dlog"
	"github.com/telepresenceio/telepresence/rpc/v2/daemon"
	"github.com/telepresenceio/telepresence/rpc/v2/manager"
	"github.com/telepresenceio/telepresence/v2/pkg/client"
	"github.com/telepresenceio/telepresence/v2/pkg/connpool"
	"github.com/telepresenceio/telepresence/v2/pkg/tun/buffer"
	"github.com/telepresenceio/telepresence/v2/pkg/tun/icmp"
	"github.com/telepresenceio/telepresence/v2/pkg/tun/ip"
	"github.com/telepresenceio/telepresence/v2/pkg/tun/tcp"
	"github.com/telepresenceio/telepresence/v2/pkg/tun/udp"
)

type Dispatcher struct {
	dev           *Device
	managerClient manager.ManagerClient
	connStream    *connpool.Stream
	handlers      *connpool.Pool
	handlersWg    sync.WaitGroup
	toTunCh       chan ip.Packet
	fragmentMap   map[uint16][]*buffer.Data
	dnsIP         net.IP
	dnsPort       uint16
	dnsLocalAddr  *net.UDPAddr
	closing       int32
	mgrConfigured <-chan struct{}
}

func NewDispatcher(dev *Device, managerConfigured <-chan struct{}) *Dispatcher {
	return &Dispatcher{
		dev:           dev,
		handlers:      connpool.NewPool(),
		toTunCh:       make(chan ip.Packet, 100),
		mgrConfigured: managerConfigured,
		fragmentMap:   make(map[uint16][]*buffer.Data),
	}
}

func (d *Dispatcher) Device() *Device {
	return d.dev
}

func (d *Dispatcher) ConfigureDNS(_ context.Context, dnsIP net.IP, dnsPort uint16, dnsLocalAddr *net.UDPAddr) error {
	d.dnsIP = dnsIP
	d.dnsPort = dnsPort
	d.dnsLocalAddr = dnsLocalAddr
	return nil
}

func (d *Dispatcher) SetManagerInfo(ctx context.Context, mi *daemon.ManagerInfo) (err error) {
	if d.managerClient == nil {
		// First check. Establish connection
		tos := &client.GetConfig(ctx).Timeouts
		tc, cancel := context.WithTimeout(ctx, tos.TrafficManagerAPI)
		defer cancel()

		var conn *grpc.ClientConn
		conn, err = grpc.DialContext(tc, fmt.Sprintf("127.0.0.1:%d", mi.GrpcPort),
			grpc.WithInsecure(),
			grpc.WithNoProxy(),
			grpc.WithBlock())
		if err != nil {
			return client.CheckTimeout(tc, &tos.TrafficManagerAPI, err)
		}
		d.managerClient = manager.NewManagerClient(conn)
	}
	return nil
}

func (d *Dispatcher) Stop(c context.Context) {
	atomic.StoreInt32(&d.closing, 1)
	d.handlers.CloseAll(c)
	d.handlersWg.Wait()
	atomic.StoreInt32(&d.closing, 2)
	d.dev.Close()
}

var blockedUDPPorts = map[uint16]bool{
	137: true, // NETBIOS Name Service
	138: true, // NETBIOS Datagram Service
	139: true, // NETBIOS
}

func (d *Dispatcher) Run(c context.Context) error {
	g := dgroup.NewGroup(c, dgroup.GroupConfig{})

	// writer
	g.Go("TUN writer", func(c context.Context) error {
		for atomic.LoadInt32(&d.closing) < 2 {
			select {
			case <-c.Done():
				return nil
			case pkt := <-d.toTunCh:
				dlog.Debugf(c, "-> TUN %s", pkt)
				_, err := d.dev.Write(pkt.Data())
				pkt.SoftRelease()
				if err != nil {
					if atomic.LoadInt32(&d.closing) == 2 || c.Err() != nil {
						err = nil
					}
					return err
				}
			}
		}
		return nil
	})

	g.Go("MGR stream", func(c context.Context) error {
		// Block here until traffic-manager tunnel is configured
		select {
		case <-c.Done():
			return nil
		case <-d.mgrConfigured:
		}

		if d.managerClient != nil {
			// TODO: ConnTunnel should probably provide a sessionID
			tcpStream, err := d.managerClient.ConnTunnel(c)
			if err != nil {
				return err
			}
			d.connStream = connpool.NewStream(tcpStream, d.handlers)
			return d.connStream.ReadLoop(c)
		}
		return nil
	})

	g.Go("TUN reader", func(c context.Context) error {
		// Block here until traffic-manager tunnel is configured
		select {
		case <-c.Done():
			return nil
		case <-d.mgrConfigured:
		}

		for atomic.LoadInt32(&d.closing) < 2 {
			data := buffer.DataPool.Get(buffer.DataPool.MTU)
			for {
				n, err := d.dev.Read(data)
				if err != nil {
					buffer.DataPool.Put(data)
					if c.Err() != nil || atomic.LoadInt32(&d.closing) == 2 {
						return nil
					}
					return fmt.Errorf("read packet error: %v", err)
				}
				if n > 0 {
					data.SetLength(n)
					break
				}
			}
			d.handlePacket(c, data)
		}
		return nil
	})
	return g.Wait()
}

func (d *Dispatcher) handlePacket(c context.Context, data *buffer.Data) {
	defer func() {
		if data != nil {
			buffer.DataPool.Put(data)
		}
	}()

	ipHdr, err := ip.ParseHeader(data.Buf())
	if err != nil {
		dlog.Error(c, "Unable to parse package header")
		return
	}

	if ipHdr.PayloadLen() > buffer.DataPool.MTU-ipHdr.HeaderLen() {
		// Package is too large for us.
		d.toTunCh <- icmp.DestinationUnreachablePacket(uint16(buffer.DataPool.MTU), ipHdr, icmp.MustFragment)
		return
	}

	if ipHdr.Version() == ipv4.Version {
		v4Hdr := ipHdr.(ip.V4Header)
		if v4Hdr.Flags()&ipv4.MoreFragments != 0 || v4Hdr.FragmentOffset() != 0 {
			data = v4Hdr.ConcatFragments(data, d.fragmentMap)
			if data == nil {
				return
			}
			v4Hdr = data.Buf()
		}
	} // TODO: similar for ipv6 using segments

	switch ipHdr.L4Protocol() {
	case unix.IPPROTO_TCP:
		d.tcp(c, tcp.PacketFromData(ipHdr, data))
		data = nil
	case unix.IPPROTO_UDP:
		dst := ipHdr.Destination()
		if dst.IsLinkLocalUnicast() || dst.IsLinkLocalMulticast() {
			// Just ignore at this point.
			return
		}
		if ip4 := dst.To4(); ip4 != nil && ip4[2] == 0 && ip4[3] == 0 {
			// Write to the a subnet's zero address. Not sure why this is happening but there's no point in
			// passing them on.
			d.toTunCh <- icmp.DestinationUnreachablePacket(uint16(buffer.DataPool.MTU), ipHdr, icmp.HostUnreachable)
			return
		}
		dg := udp.DatagramFromData(ipHdr, data)
		if blockedUDPPorts[dg.Header().SourcePort()] || blockedUDPPorts[dg.Header().DestinationPort()] {
			d.toTunCh <- icmp.DestinationUnreachablePacket(uint16(buffer.DataPool.MTU), ipHdr, icmp.PortUnreachable)
			return
		}
		data = nil
		d.udp(c, dg)
	case unix.IPPROTO_ICMP:
	case unix.IPPROTO_ICMPV6:
		pkt := icmp.PacketFromData(ipHdr, data)
		dlog.Debugf(c, "<- TUN %s", pkt)
	default:
		// An L4 protocol that we don't handle.
		d.toTunCh <- icmp.DestinationUnreachablePacket(uint16(buffer.DataPool.MTU), ipHdr, icmp.ProtocolUnreachable)
	}
}

func (d *Dispatcher) tcp(c context.Context, pkt tcp.Packet) {
	dlog.Debugf(c, "<- TUN %s", pkt)
	ipHdr := pkt.IPHeader()
	tcpHdr := pkt.Header()
	connID := connpool.NewConnID(unix.IPPROTO_TCP, ipHdr.Source(), ipHdr.Destination(), tcpHdr.SourcePort(), tcpHdr.DestinationPort())
	wf, err := d.handlers.Get(c, connID, func(c context.Context, remove func()) (connpool.Handler, error) {
		if tcpHdr.RST() {
			return nil, errors.New("dispatching got RST without connection workflow")
		}
		if !tcpHdr.SYN() {
			select {
			case <-c.Done():
				return nil, c.Err()
			case d.toTunCh <- pkt.Reset():
			}
		}
		d.handlersWg.Add(1)
		return tcp.NewHandler(c, &d.handlersWg, d.connStream, &d.closing, d.toTunCh, connID, remove), nil
	})
	if err != nil {
		dlog.Error(c, err)
		return
	}
	wf.(tcp.PacketHandler).HandlePacket(c, pkt)
}

func (d *Dispatcher) udp(c context.Context, dg udp.Datagram) {
	dlog.Debugf(c, "<- TUN %s", dg)
	ipHdr := dg.IPHeader()
	udpHdr := dg.Header()
	connID := connpool.NewConnID(unix.IPPROTO_UDP, ipHdr.Source(), ipHdr.Destination(), udpHdr.SourcePort(), udpHdr.DestinationPort())
	uh, err := d.handlers.Get(c, connID, func(c context.Context, remove func()) (connpool.Handler, error) {
		d.handlersWg.Add(1)
		if udpHdr.DestinationPort() == d.dnsPort && ipHdr.Destination().Equal(d.dnsIP) {
			return udp.NewDnsInterceptor(c, &d.handlersWg, d.connStream, d.toTunCh, connID, remove, d.dnsLocalAddr)
		}
		return udp.NewHandler(c, &d.handlersWg, d.connStream, d.toTunCh, connID, remove), nil
	})
	if err != nil {
		dlog.Error(c, err)
		return
	}
	uh.(udp.DatagramHandler).NewDatagram(c, dg)
}

func (d *Dispatcher) AddSubnets(c context.Context, subnets []*net.IPNet) error {
	for _, sn := range subnets {
		dlog.Debugf(c, "Adding subnet %s", sn)
		if err := d.dev.AddSubnet(c, sn); err != nil {
			return err
		}
	}
	return nil
}
