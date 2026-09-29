package bluetooth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	esphome "github.com/ygelfand/go-esphome-device"
	"github.com/ygelfand/go-esphome-device/api"

	"github.com/ygelfand/echolocal/internal/hardware/ble"
)

// connectionSlots is how many devices Home Assistant may hold connections to at once, which is
// what ESPHome defaults to on an ESP32.
const connectionSlots = 3

// connectAttempts is how often a link that fails to establish is tried again within one request.
const connectAttempts = 3

// statusFailedToEstablish is the HCI status of a link that came up but never carried a packet.
const statusFailedToEstablish = 0x3E

// connectTimeout bounds one request. Home Assistant gives up on its own side at about the same time
// and asks again.
const connectTimeout = 20 * time.Second

// Error codes as ESPHome reports them: HCI reasons where there is one, otherwise ESP-IDF's.
const (
	errGATT         = 0x85
	errConnCancel   = 0x0100
	errNoConnection = 0x0101
	errNoSlot       = 0x09 // HCI connection limit exceeded
)

// gatt proxies connections: Home Assistant names a device, this side connects to it and relays
// GATT operations by handle. Home Assistant caches services itself (remote caching), so after a
// connect it may go straight to reads and writes, and it writes CCCDs itself to turn on
// notifications.
type gatt struct {
	radio *ble.Radio

	mu    sync.Mutex
	slots map[uint64]*slot
	// free is the connection that asked to hear about free slots.
	free *esphome.Conn
}

type slot struct {
	api    *esphome.Conn
	cancel context.CancelFunc
	link   *ble.Conn
	// notify is the handles Home Assistant subscribed to.
	notify map[uint16]bool
}

func newGATT(radio *ble.Radio) *gatt { return &gatt{radio: radio, slots: map[uint64]*slot{}} }

// handle answers connection and GATT messages, and reports whether msg was one.
func (g *gatt) handle(conn *esphome.Conn, msg proto.Message) (bool, error) {
	switch m := msg.(type) {
	case *api.SubscribeBluetoothConnectionsFreeRequest:
		g.mu.Lock()
		g.free = conn
		g.mu.Unlock()
		return true, conn.Send(g.freeResponse())

	case *api.BluetoothDeviceRequest:
		return true, g.device(conn, m)

	case *api.BluetoothGATTGetServicesRequest:
		go g.services(conn, m.GetAddress())
	case *api.BluetoothGATTReadRequest:
		go g.read(conn, m.GetAddress(), m.GetHandle())
	case *api.BluetoothGATTReadDescriptorRequest:
		go g.read(conn, m.GetAddress(), m.GetHandle())
	case *api.BluetoothGATTWriteRequest:
		go g.write(conn, m.GetAddress(), m.GetHandle(), m.GetData(), m.GetResponse())
	case *api.BluetoothGATTWriteDescriptorRequest:
		go g.write(conn, m.GetAddress(), m.GetHandle(), m.GetData(), true)
	case *api.BluetoothGATTNotifyRequest:
		return true, g.subscribe(conn, m.GetAddress(), m.GetHandle(), m.GetEnable())
	default:
		return false, nil
	}
	return true, nil
}

func (g *gatt) device(conn *esphome.Conn, m *api.BluetoothDeviceRequest) error {
	address := m.GetAddress()
	switch m.GetRequestType() {
	case api.BluetoothDeviceRequestType_BLUETOOTH_DEVICE_REQUEST_TYPE_CONNECT_V3_WITH_CACHE,
		api.BluetoothDeviceRequestType_BLUETOOTH_DEVICE_REQUEST_TYPE_CONNECT_V3_WITHOUT_CACHE,
		api.BluetoothDeviceRequestType_BLUETOOTH_DEVICE_REQUEST_TYPE_CONNECT:
		return g.connect(conn, address, uint8(m.GetAddressType()))

	case api.BluetoothDeviceRequestType_BLUETOOTH_DEVICE_REQUEST_TYPE_DISCONNECT:
		g.mu.Lock()
		s := g.slots[address]
		g.mu.Unlock()
		if s == nil {
			return conn.Send(&api.BluetoothDeviceConnectionResponse{Address: address, Error: errNoConnection})
		}
		s.cancel()
		if s.link != nil {
			go func() { _ = s.link.Disconnect() }()
		}
		return nil

	case api.BluetoothDeviceRequestType_BLUETOOTH_DEVICE_REQUEST_TYPE_PAIR:
		// Pairing needs keys kept across boots, which this proxy does not keep.
		return conn.Send(&api.BluetoothDevicePairingResponse{Address: address, Error: errGATT})
	case api.BluetoothDeviceRequestType_BLUETOOTH_DEVICE_REQUEST_TYPE_UNPAIR:
		return conn.Send(&api.BluetoothDeviceUnpairingResponse{Address: address, Success: true})
	case api.BluetoothDeviceRequestType_BLUETOOTH_DEVICE_REQUEST_TYPE_CLEAR_CACHE:
		// Nothing is cached on this side.
		return conn.Send(&api.BluetoothDeviceClearCacheResponse{Address: address, Success: true})
	}
	return nil
}

func (g *gatt) connect(conn *esphome.Conn, address uint64, addrType uint8) error {
	if !g.radio.Running() {
		return conn.Send(&api.BluetoothDeviceConnectionResponse{Address: address, Error: errGATT})
	}

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	g.mu.Lock()
	if _, busy := g.slots[address]; busy || len(g.slots) >= connectionSlots {
		g.mu.Unlock()
		cancel()
		code := int32(errNoSlot)
		if busy {
			code = errConnCancel
		}
		return conn.Send(&api.BluetoothDeviceConnectionResponse{Address: address, Error: code})
	}
	s := &slot{api: conn, cancel: cancel, notify: map[uint16]bool{}}
	g.slots[address] = s
	g.mu.Unlock()
	g.announceFree()

	go g.dial(ctx, s, address, addrType)
	return nil
}

func (g *gatt) dial(ctx context.Context, s *slot, address uint64, addrType uint8) {
	defer s.cancel()

	link, err := g.radio.Connect(ctx, macBytes(address), addrType)
	var status ble.StatusError
	for attempt := 1; attempt < connectAttempts && errors.As(err, &status) && status == statusFailedToEstablish && ctx.Err() == nil; attempt++ {
		// A link that fails to establish is the radio missing the first exchanges, not the device
		// refusing: it usually comes up at the next try.
		link, err = g.radio.Connect(ctx, macBytes(address), addrType)
	}
	if err != nil {
		code := int32(errGATT)
		switch {
		case errors.As(err, &status):
			code = int32(status)
		case errors.Is(err, context.Canceled):
			code = errConnCancel
		}
		slog.Info("ble proxy connection failed", "address", mac(address), "err", err)
		_ = s.api.Send(&api.BluetoothDeviceConnectionResponse{Address: address, Error: code})
		g.release(address, s)
		return
	}

	link.OnNotify(func(handle uint16, data []byte) {
		g.mu.Lock()
		on := s.notify[handle]
		g.mu.Unlock()
		if on {
			// On the radio's reader: a slow client costs a notification, never the reader.
			go func() {
				_ = s.api.Send(&api.BluetoothGATTNotifyDataResponse{Address: address, Handle: uint32(handle), Data: data})
			}()
		}
	})

	g.mu.Lock()
	s.link = link
	cancelled := ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded)
	g.mu.Unlock()

	if cancelled {
		// Home Assistant asked to disconnect while the link was coming up.
		_ = link.Disconnect()
	} else if err := s.api.Send(&api.BluetoothDeviceConnectionResponse{
		Address: address, Connected: true, Mtu: uint32(link.MTU()),
	}); err != nil {
		// Home Assistant is gone, and a link nobody can use only holds a slot.
		_ = link.Disconnect()
	}

	<-link.Done()
	// The connection's end first, then the freed slot: a slot freed while Home Assistant still
	// holds the connection reads to it as a stale one, and it drops it with a warning.
	_ = s.api.Send(&api.BluetoothDeviceConnectionResponse{Address: address, Error: int32(link.Reason())})
	g.release(address, s)
}

func (g *gatt) release(address uint64, s *slot) {
	g.mu.Lock()
	if g.slots[address] == s {
		delete(g.slots, address)
	}
	g.mu.Unlock()
	g.announceFree()
}

// link is the connection to a device, if there is one up.
func (g *gatt) link(address uint64) *slot {
	g.mu.Lock()
	defer g.mu.Unlock()
	if s := g.slots[address]; s != nil && s.link != nil {
		return s
	}
	return nil
}

func (g *gatt) services(conn *esphome.Conn, address uint64) {
	s := g.link(address)
	if s == nil {
		_ = conn.Send(&api.BluetoothGATTErrorResponse{Address: address, Error: errNoConnection})
		return
	}
	services, err := s.link.Discover(context.Background())
	if err != nil {
		_ = conn.Send(&api.BluetoothGATTErrorResponse{Address: address, Error: gattError(err)})
		return
	}
	// One service per message, as ESPHome sends them: a large table would not fit one frame.
	for _, sv := range services {
		if err := conn.Send(&api.BluetoothGATTGetServicesResponse{Address: address, Services: []*api.BluetoothGATTService{service(sv)}}); err != nil {
			return
		}
	}
	_ = conn.Send(&api.BluetoothGATTGetServicesDoneResponse{Address: address})
}

func (g *gatt) read(conn *esphome.Conn, address uint64, handle uint32) {
	s := g.link(address)
	if s == nil {
		_ = conn.Send(&api.BluetoothGATTErrorResponse{Address: address, Handle: handle, Error: errNoConnection})
		return
	}
	data, err := s.link.Read(context.Background(), uint16(handle))
	if err != nil {
		_ = conn.Send(&api.BluetoothGATTErrorResponse{Address: address, Handle: handle, Error: gattError(err)})
		return
	}
	_ = conn.Send(&api.BluetoothGATTReadResponse{Address: address, Handle: handle, Data: data})
}

func (g *gatt) write(conn *esphome.Conn, address uint64, handle uint32, data []byte, response bool) {
	s := g.link(address)
	if s == nil {
		_ = conn.Send(&api.BluetoothGATTErrorResponse{Address: address, Handle: handle, Error: errNoConnection})
		return
	}
	if err := s.link.Write(context.Background(), uint16(handle), data, response); err != nil {
		_ = conn.Send(&api.BluetoothGATTErrorResponse{Address: address, Handle: handle, Error: gattError(err)})
		return
	}
	// ESPHome confirms every write, with response or without, and Home Assistant waits for it.
	_ = conn.Send(&api.BluetoothGATTWriteResponse{Address: address, Handle: handle})
}

// subscribe records which notifications to pass on. Under remote caching the CCCD write that turns
// them on at the device is Home Assistant's own, sent as a descriptor write.
func (g *gatt) subscribe(conn *esphome.Conn, address uint64, handle uint32, enable bool) error {
	s := g.link(address)
	if s == nil {
		return conn.Send(&api.BluetoothGATTErrorResponse{Address: address, Handle: handle, Error: errNoConnection})
	}
	g.mu.Lock()
	if enable {
		s.notify[uint16(handle)] = true
	} else {
		delete(s.notify, uint16(handle))
	}
	g.mu.Unlock()
	return conn.Send(&api.BluetoothGATTNotifyResponse{Address: address, Handle: handle})
}

// closeAll drops every connection, when the proxy is turned off or Home Assistant goes.
func (g *gatt) closeAll() {
	g.mu.Lock()
	slots := make([]*slot, 0, len(g.slots))
	for _, s := range g.slots {
		slots = append(slots, s)
	}
	g.mu.Unlock()
	for _, s := range slots {
		s.cancel()
		if s.link != nil {
			_ = s.link.Disconnect()
		}
	}
}

func (g *gatt) freeResponse() *api.BluetoothConnectionsFreeResponse {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := &api.BluetoothConnectionsFreeResponse{Free: uint32(connectionSlots - len(g.slots)), Limit: connectionSlots}
	for address := range g.slots {
		r.Allocated = append(r.Allocated, address)
	}
	return r
}

// announceFree tells Home Assistant how many slots it has, which it checks before asking for one.
func (g *gatt) announceFree() {
	g.mu.Lock()
	conn := g.free
	g.mu.Unlock()
	if conn != nil {
		if err := conn.Send(g.freeResponse()); err != nil {
			g.mu.Lock()
			if g.free == conn {
				g.free = nil
			}
			g.mu.Unlock()
		}
	}
}

// gattError is the code Home Assistant is told: the ATT error where the device gave one.
func gattError(err error) int32 {
	var ae *ble.ATTError
	if errors.As(err, &ae) {
		return int32(ae.Code)
	}
	if errors.Is(err, ble.ErrClosed) {
		return errNoConnection
	}
	return errGATT
}

func service(s ble.Service) *api.BluetoothGATTService {
	out := &api.BluetoothGATTService{Uuid: halves(s.UUID), Handle: uint32(s.Handle)}
	for _, c := range s.Characteristics {
		ch := &api.BluetoothGATTCharacteristic{Uuid: halves(c.UUID), Handle: uint32(c.Handle), Properties: uint32(c.Properties)}
		for _, d := range c.Descriptors {
			ch.Descriptors = append(ch.Descriptors, &api.BluetoothGATTDescriptor{Uuid: halves(d.UUID), Handle: uint32(d.Handle)})
		}
		out.Characteristics = append(out.Characteristics, ch)
	}
	return out
}

func halves(u ble.UUID) []uint64 {
	high, low := u.Halves()
	return []uint64{high, low}
}

// macBytes is an ESPHome address, a big-endian integer, as the six bytes it is written as.
func macBytes(address uint64) [6]byte {
	var b [6]byte
	for i := range 6 {
		b[i] = byte(address >> (8 * (5 - i)))
	}
	return b
}

func mac(address uint64) string { return fmt.Sprintf("%012X", address) }
