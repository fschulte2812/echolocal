package bluetooth

import (
	"errors"
	"testing"

	"github.com/ygelfand/echolocal/internal/hardware/ble"
)

func TestMacBytes(t *testing.T) {
	got := macBytes(0x685edd16f8e2)
	want := [6]byte{0x68, 0x5e, 0xdd, 0x16, 0xf8, 0xe2}
	if got != want {
		t.Errorf("macBytes = %x, want %x", got, want)
	}
}

func TestServiceMessage(t *testing.T) {
	s := ble.Service{
		UUID:   ble.Short(0x180a),
		Handle: 10,
		Characteristics: []ble.Characteristic{{
			UUID: ble.Short(0x2a29), Handle: 12, Properties: 0x12,
			Descriptors: []ble.Descriptor{{UUID: ble.Short(0x2902), Handle: 13}},
		}},
	}
	m := service(s)
	if m.GetHandle() != 10 || len(m.GetUuid()) != 2 || m.GetUuid()[0] != 0x0000180a00001000 || m.GetUuid()[1] != 0x800000805f9b34fb {
		t.Fatalf("service = %v", m)
	}
	c := m.GetCharacteristics()[0]
	if c.GetHandle() != 12 || c.GetProperties() != 0x12 || c.GetDescriptors()[0].GetHandle() != 13 {
		t.Errorf("characteristic = %v", c)
	}
}

func TestGATTError(t *testing.T) {
	if got := gattError(&ble.ATTError{Code: 0x02}); got != 0x02 {
		t.Errorf("att error = %#x, want the device's own code", got)
	}
	if got := gattError(ble.ErrClosed); got != errNoConnection {
		t.Errorf("closed = %#x", got)
	}
	if got := gattError(errors.New("timeout")); got != errGATT {
		t.Errorf("other = %#x", got)
	}
}

func TestFreeSlots(t *testing.T) {
	g := newGATT(nil)
	g.slots[1] = &slot{}
	r := g.freeResponse()
	if r.GetFree() != connectionSlots-1 || r.GetLimit() != connectionSlots || len(r.GetAllocated()) != 1 {
		t.Errorf("free = %v", r)
	}
}
