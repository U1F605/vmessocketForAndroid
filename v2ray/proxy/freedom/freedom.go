package freedom

//go:generate go run github.com/v2fly/v2ray-core/v5/common/errors/errorgen

import (
	"context"
	"time"

	core "github.com/v2fly/v2ray-core/v5"
	"github.com/v2fly/v2ray-core/v5/common"
	"github.com/v2fly/v2ray-core/v5/common/buf"
	"github.com/v2fly/v2ray-core/v5/common/dice"
	"github.com/v2fly/v2ray-core/v5/common/net"
	"github.com/v2fly/v2ray-core/v5/common/retry"
	"github.com/v2fly/v2ray-core/v5/common/session"
	"github.com/v2fly/v2ray-core/v5/common/signal"
	"github.com/v2fly/v2ray-core/v5/common/task"
	"github.com/v2fly/v2ray-core/v5/features/dns"
	"github.com/v2fly/v2ray-core/v5/features/policy"
	"github.com/v2fly/v2ray-core/v5/features/stats"
	"github.com/v2fly/v2ray-core/v5/nekoutils"
	"github.com/v2fly/v2ray-core/v5/transport"
	"github.com/v2fly/v2ray-core/v5/transport/internet"
)

func init() {
	common.Must(common.RegisterConfig((*Config)(nil), func(ctx context.Context, config interface{}) (interface{}, error) {
		h := new(Handler)
		if err := core.RequireFeatures(ctx, func(pm policy.Manager, d dns.Client) error {
			return h.Init(config.(*Config), pm, d)
		}); err != nil {
			return nil, err
		}
		return h, nil
	}))

	common.Must(common.RegisterConfig((*SimplifiedConfig)(nil), func(ctx context.Context, config interface{}) (interface{}, error) {
		simplifiedServer := config.(*SimplifiedConfig)
		_ = simplifiedServer
		fullConfig := &Config{}
		return common.CreateObject(ctx, fullConfig)
	}))
}

// Handler handles Freedom connections.
type Handler struct {
	policyManager policy.Manager
	dns           dns.Client
	config        *Config
}

// Init initializes the Handler with necessary parameters.
func (h *Handler) Init(config *Config, pm policy.Manager, d dns.Client) error {
	h.config = config
	h.policyManager = pm
	h.dns = d

	return nil
}

func (h *Handler) policy() policy.Session {
	p := h.policyManager.ForLevel(h.config.UserLevel)
	if h.config.Timeout > 0 && h.config.UserLevel == 0 {
		p.Timeouts.ConnectionIdle = time.Duration(h.config.Timeout) * time.Second
	}
	return p
}

func (h *Handler) resolveIP(ctx context.Context, domain string, localAddr net.Address) net.Address {
	if c, ok := h.dns.(dns.ClientWithIPOption); ok {
		c.SetFakeDNSOption(false) // Skip FakeDNS
	} else {
		newError("DNS client doesn't implement ClientWithIPOption")
	}

	lookupFunc := h.dns.LookupIP
	if h.config.DomainStrategy == Config_USE_IP4 || (localAddr != nil && localAddr.Family().IsIPv4()) {
		if lookupIPv4, ok := h.dns.(dns.IPv4Lookup); ok {
			lookupFunc = lookupIPv4.LookupIPv4
		}
	} else if h.config.DomainStrategy == Config_USE_IP6 || (localAddr != nil && localAddr.Family().IsIPv6()) {
		if lookupIPv6, ok := h.dns.(dns.IPv6Lookup); ok {
			lookupFunc = lookupIPv6.LookupIPv6
		}
	}

	ips, err := lookupFunc(domain)
	if err != nil {
		newError("failed to get IP address for domain ", domain).Base(err).WriteToLog(session.ExportIDToError(ctx))
	}
	if len(ips) == 0 {
		return nil
	}
	return net.IPAddress(ips[dice.Roll(len(ips))])
}

func isValidAddress(addr *net.IPOrDomain) bool {
	if addr == nil {
		return false
	}

	a := addr.AsAddress()
	return a != net.AnyIP
}

// Process implements proxy.Outbound.
func (h *Handler) Process(ctx context.Context, link *transport.Link, dialer internet.Dialer) error {
	outbound := session.OutboundFromContext(ctx)
	if outbound == nil || !outbound.Target.IsValid() {
		return newError("target not specified.")
	}
	destination := outbound.Target
	if h.config.DestinationOverride != nil {
		server := h.config.DestinationOverride.Server
		if isValidAddress(server.Address) {
			destination.Address = server.Address.AsAddress()
		}
		if server.Port != 0 {
			destination.Port = net.Port(server.Port)
		}
	}
	newError("opening connection to ", destination).WriteToLog(session.ExportIDToError(ctx))

	input := link.Reader
	output := link.Writer

	var conn internet.Connection
	err := retry.ExponentialBackoff(5, 100).On(func() error {
		dialDest := destination
		if h.config.useIP() && dialDest.Address.Family().IsDomain() {
			ip := h.resolveIP(ctx, dialDest.Address.Domain(), dialer.Address())
			if ip != nil {
				dialDest = net.Destination{
					Network: dialDest.Network,
					Address: ip,
					Port:    dialDest.Port,
				}
				newError("dialing to ", dialDest).WriteToLog(session.ExportIDToError(ctx))
			}
		}

		rawConn, err := dialer.Dial(ctx, dialDest)
		if err != nil {
			return err
		}
		conn = rawConn
		return nil
	})
	if err != nil {
		return newError("failed to open connection to ", destination).Base(err)
	}
	defer conn.Close()

	plcy := h.policy()
	ctx, cancel := context.WithCancel(ctx)
	timer := signal.CancelAfterInactivity(ctx, cancel, plcy.Timeouts.ConnectionIdle)

	requestDone := func() error {
		defer timer.SetTimeout(plcy.Timeouts.DownlinkOnly)

		var writer buf.Writer
		if destination.Network == net.Network_TCP {
			writer = buf.NewWriter(conn)
		} else {
			writer = newPacketWriter(conn)
		}

		if err := buf.Copy(input, writer, buf.UpdateActivity(timer)); err != nil {
			return newError("failed to process request").Base(err)
		}

		return nil
	}

	responseDone := func() error {
		defer timer.SetTimeout(plcy.Timeouts.UplinkOnly)

		var reader buf.Reader
		if destination.Network == net.Network_TCP {
			reader = buf.NewReader(conn)
		} else {
			reader = newPacketReader(conn)
		}
		if err := buf.Copy(reader, output, buf.UpdateActivity(timer)); err != nil {
			return newError("failed to process response").Base(err)
		}

		return nil
	}

	if err := task.Run(ctx, requestDone, task.OnSuccess(responseDone, task.Close(output))); err != nil {
		return newError("connection ends").Base(err)
	}

	return nil
}

// nekohasekai begin

func newPacketReader(conn net.Conn) buf.Reader {
	iConn := conn
	statConn, ok := iConn.(*internet.StatCouterConnection)
	if ok {
		iConn = statConn.Connection
	}
	var counter stats.Counter
	if statConn != nil {
		counter = statConn.ReadCounter
	}
	if c, ok := iConn.(nekoutils.FusedConn); ok {
		return &packetReader{
			c, counter,
		}
	}
	return &buf.PacketReader{Reader: conn}
}

type packetReader struct {
	conn    nekoutils.FusedConn
	counter stats.Counter
}

func (r *packetReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	b := buf.New()
	b.Resize(0, buf.Size)
	n, d, err := r.conn.ReadFrom(b.Bytes())
	if err != nil {
		b.Release()
		return nil, err
	}
	b.Resize(0, int32(n))
	b.Endpoint = &net.Destination{
		Address: net.IPAddress(d.(*net.UDPAddr).IP),
		Port:    net.Port(d.(*net.UDPAddr).Port),
		Network: net.Network_UDP,
	}
	if r.counter != nil {
		r.counter.Add(int64(n))
	}
	return buf.MultiBuffer{b}, nil
}

func newPacketWriter(conn net.Conn) buf.Writer {
	iConn := conn
	statConn, ok := iConn.(*internet.StatCouterConnection)
	if ok {
		iConn = statConn.Connection
	}
	var counter stats.Counter
	if statConn != nil {
		counter = statConn.WriteCounter
	}
	if c, ok := iConn.(nekoutils.FusedConn); ok {
		return &packetWriter{
			c,
			counter,
		}
	}
	return &buf.SequentialWriter{Writer: conn}
}

type packetWriter struct {
	conn    nekoutils.FusedConn
	counter stats.Counter
}

func (w *packetWriter) WriteMultiBuffer(mb buf.MultiBuffer) (err error) {
	for _, buffer := range mb {
		var n int
		if buffer.Endpoint != nil {
			destAddr, ex := net.ResolveUDPAddr("udp", buffer.Endpoint.NetAddr())
			if ex != nil {
				err = ex
			} else {
				n, err = w.conn.WriteTo(buffer.Bytes(), destAddr)
			}
		} else {
			n, err = w.conn.Write(buffer.Bytes())
		}
		if err != nil {
			buf.ReleaseMulti(mb)
			return err
		}
		buffer.Release()
		if w.counter != nil {
			w.counter.Add(int64(n))
		}
	}
	return nil
}
