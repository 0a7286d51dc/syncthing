package model

import (
	"bytes"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/calmh/syncthing/protocol"
)

func TestNewModel(t *testing.T) {
	m := NewModel("foo")

	if m == nil {
		t.Fatalf("NewModel returned nil")
	}

	if len(m.need) > 0 {
		t.Errorf("New model should have no Need")
	}

	if len(m.local) > 0 {
		t.Errorf("New model should have no Have")
	}
}

var testDataExpected = map[string]File{
	"foo": File{
		Name:     "foo",
		Flags:    0,
		Modified: 0,
		Blocks:   []Block{{Offset: 0x0, Length: 0x7, Hash: []uint8{0xae, 0xc0, 0x70, 0x64, 0x5f, 0xe5, 0x3e, 0xe3, 0xb3, 0x76, 0x30, 0x59, 0x37, 0x61, 0x34, 0xf0, 0x58, 0xcc, 0x33, 0x72, 0x47, 0xc9, 0x78, 0xad, 0xd1, 0x78, 0xb6, 0xcc, 0xdf, 0xb0, 0x1, 0x9f}}},
	},
	"bar": File{
		Name:     "bar",
		Flags:    0,
		Modified: 0,
		Blocks:   []Block{{Offset: 0x0, Length: 0xa, Hash: []uint8{0x2f, 0x72, 0xcc, 0x11, 0xa6, 0xfc, 0xd0, 0x27, 0x1e, 0xce, 0xf8, 0xc6, 0x10, 0x56, 0xee, 0x1e, 0xb1, 0x24, 0x3b, 0xe3, 0x80, 0x5b, 0xf9, 0xa9, 0xdf, 0x98, 0xf9, 0x2f, 0x76, 0x36, 0xb0, 0x5c}}},
	},
	"baz": File{
		Name:     "baz",
		Flags:    protocol.FlagDirectory,
		Modified: 0,
		Blocks:   nil,
	},
	"baz/quux": File{
		Name:     "baz/quux",
		Flags:    0,
		Modified: 0,
		Blocks:   []Block{{Offset: 0x0, Length: 0x9, Hash: []uint8{0xc1, 0x54, 0xd9, 0x4e, 0x94, 0xba, 0x72, 0x98, 0xa6, 0xad, 0xb0, 0x52, 0x3a, 0xfe, 0x34, 0xd1, 0xb6, 0xa5, 0x81, 0xd6, 0xb8, 0x93, 0xa7, 0x63, 0xd4, 0x5d, 0xdc, 0x5e, 0x20, 0x9d, 0xcb, 0x83}}},
	},
}

func init() {
	// Fix expected test data to match reality
	for n, f := range testDataExpected {
		fi, _ := os.Stat("testdata/" + n)
		f.Flags = f.Flags&^0xfff | uint32(fi.Mode()&0xfff)
		f.Modified = fi.ModTime().Unix()
		testDataExpected[n] = f
	}
}

func TestUpdateLocal(t *testing.T) {
	m := NewModel("testdata")
	fs, _ := m.Walk(false)
	m.ReplaceLocal(fs)

	if len(m.need) > 0 {
		t.Fatalf("Model with only local data should have no need")
	}

	if l1, l2 := len(m.local), len(testDataExpected); l1 != l2 {
		t.Errorf("Model len(local) incorrect, %d != %d", l1, l2)
	} else {
		for i := range testDataExpected {
			if !reflect.DeepEqual(testDataExpected[i], m.local[i]) {
				t.Errorf("Local file %d mismatch\n  E: %+v\n  A: %+v\n", i, testDataExpected[i], m.local[i])
			}
		}
	}

	if l1, l2 := len(m.global), len(testDataExpected); l1 != l2 {
		t.Errorf("Model len(global) incorrect, %d != %d", l1, l2)
	} else {
		for i := range testDataExpected {
			if !reflect.DeepEqual(testDataExpected[i], m.global[i]) {
				t.Errorf("Local file %d mismatch\n  E: %+v\n  A: %+v\n", i, testDataExpected[i], m.global[i])
			}
		}
	}
}

func TestRemoteUpdateExisting(t *testing.T) {
	m := NewModel("testdata")
	fs, _ := m.Walk(false)
	m.ReplaceLocal(fs)

	newFile := protocol.FileInfo{
		Name:     "foo",
		Modified: time.Now().Unix(),
		Blocks:   []protocol.BlockInfo{{100, []byte("some hash bytes")}},
	}
	m.Index("42", []protocol.FileInfo{newFile})

	if l := len(m.need); l != 1 {
		t.Errorf("Model missing Need for one file (%d != 1)", l)
	}
}

func TestRemoteAddNew(t *testing.T) {
	m := NewModel("testdata")
	fs, _ := m.Walk(false)
	m.ReplaceLocal(fs)

	newFile := protocol.FileInfo{
		Name:     "a new file",
		Modified: time.Now().Unix(),
		Blocks:   []protocol.BlockInfo{{100, []byte("some hash bytes")}},
	}
	m.Index("42", []protocol.FileInfo{newFile})

	if l1, l2 := len(m.need), 1; l1 != l2 {
		t.Errorf("Model len(m.need) incorrect (%d != %d)", l1, l2)
	}
}

func TestRemoteUpdateOld(t *testing.T) {
	m := NewModel("testdata")
	fs, _ := m.Walk(false)
	m.ReplaceLocal(fs)

	oldTimeStamp := int64(1234)
	newFile := protocol.FileInfo{
		Name:     "foo",
		Modified: oldTimeStamp,
		Blocks:   []protocol.BlockInfo{{100, []byte("some hash bytes")}},
	}
	m.Index("42", []protocol.FileInfo{newFile})

	if l1, l2 := len(m.need), 0; l1 != l2 {
		t.Errorf("Model len(need) incorrect (%d != %d)", l1, l2)
	}
}

func TestRemoteIndexUpdate(t *testing.T) {
	m := NewModel("testdata")
	fs, _ := m.Walk(false)
	m.ReplaceLocal(fs)

	foo := protocol.FileInfo{
		Name:     "foo",
		Modified: time.Now().Unix(),
		Blocks:   []protocol.BlockInfo{{100, []byte("some hash bytes")}},
	}

	bar := protocol.FileInfo{
		Name:     "bar",
		Modified: time.Now().Unix(),
		Blocks:   []protocol.BlockInfo{{100, []byte("some hash bytes")}},
	}

	m.Index("42", []protocol.FileInfo{foo})

	if _, ok := m.need["foo"]; !ok {
		t.Error("Model doesn't need 'foo'")
	}

	m.IndexUpdate("42", []protocol.FileInfo{bar})

	if _, ok := m.need["foo"]; !ok {
		t.Error("Model doesn't need 'foo'")
	}
	if _, ok := m.need["bar"]; !ok {
		t.Error("Model doesn't need 'bar'")
	}
}

func TestDelete(t *testing.T) {
	m := NewModel("testdata")
	fs, _ := m.Walk(false)
	m.ReplaceLocal(fs)

	if l1, l2 := len(m.local), len(fs); l1 != l2 {
		t.Errorf("Model len(local) incorrect (%d != %d)", l1, l2)
	}
	if l1, l2 := len(m.global), len(fs); l1 != l2 {
		t.Errorf("Model len(global) incorrect (%d != %d)", l1, l2)
	}

	ot := time.Now().Unix()
	newFile := File{
		Name:     "a new file",
		Modified: ot,
		Blocks:   []Block{{0, 100, []byte("some hash bytes")}},
	}
	m.updateLocal(newFile)

	if l1, l2 := len(m.local), len(fs)+1; l1 != l2 {
		t.Errorf("Model len(local) incorrect (%d != %d)", l1, l2)
	}
	if l1, l2 := len(m.global), len(fs)+1; l1 != l2 {
		t.Errorf("Model len(global) incorrect (%d != %d)", l1, l2)
	}

	// The deleted file is kept in the local and global tables and marked as deleted.

	m.ReplaceLocal(fs)

	if l1, l2 := len(m.local), len(fs)+1; l1 != l2 {
		t.Errorf("Model len(local) incorrect (%d != %d)", l1, l2)
	}
	if l1, l2 := len(m.global), len(fs)+1; l1 != l2 {
		t.Errorf("Model len(global) incorrect (%d != %d)", l1, l2)
	}

	if m.local["a new file"].Flags&(1<<12) == 0 {
		t.Error("Unexpected deleted flag = 0 in local table")
	}
	if len(m.local["a new file"].Blocks) != 0 {
		t.Error("Unexpected non-zero blocks for deleted file in local")
	}
	if ft := m.local["a new file"].Modified; ft != ot {
		t.Errorf("Unexpected time %d != %d for deleted file in local", ft, ot+1)
	}
	if fv := m.local["a new file"].Version; fv != 1 {
		t.Errorf("Unexpected version %d != 1 for deleted file in local", fv)
	}

	if m.global["a new file"].Flags&(1<<12) == 0 {
		t.Error("Unexpected deleted flag = 0 in global table")
	}
	if len(m.global["a new file"].Blocks) != 0 {
		t.Error("Unexpected non-zero blocks for deleted file in global")
	}
	if ft := m.global["a new file"].Modified; ft != ot {
		t.Errorf("Unexpected time %d != %d for deleted file in global", ft, ot+1)
	}
	if fv := m.local["a new file"].Version; fv != 1 {
		t.Errorf("Unexpected version %d != 1 for deleted file in global", fv)
	}

	// Another update should change nothing

	m.ReplaceLocal(fs)

	if l1, l2 := len(m.local), len(fs)+1; l1 != l2 {
		t.Errorf("Model len(local) incorrect (%d != %d)", l1, l2)
	}
	if l1, l2 := len(m.global), len(fs)+1; l1 != l2 {
		t.Errorf("Model len(global) incorrect (%d != %d)", l1, l2)
	}

	if m.local["a new file"].Flags&(1<<12) == 0 {
		t.Error("Unexpected deleted flag = 0 in local table")
	}
	if len(m.local["a new file"].Blocks) != 0 {
		t.Error("Unexpected non-zero blocks for deleted file in local")
	}
	if ft := m.local["a new file"].Modified; ft != ot {
		t.Errorf("Unexpected time %d != %d for deleted file in local", ft, ot)
	}
	if fv := m.local["a new file"].Version; fv != 1 {
		t.Errorf("Unexpected version %d != 1 for deleted file in local", fv)
	}

	if m.global["a new file"].Flags&(1<<12) == 0 {
		t.Error("Unexpected deleted flag = 0 in global table")
	}
	if len(m.global["a new file"].Blocks) != 0 {
		t.Error("Unexpected non-zero blocks for deleted file in global")
	}
	if ft := m.global["a new file"].Modified; ft != ot {
		t.Errorf("Unexpected time %d != %d for deleted file in global", ft, ot)
	}
	if fv := m.local["a new file"].Version; fv != 1 {
		t.Errorf("Unexpected version %d != 1 for deleted file in global", fv)
	}
}

func TestForgetNode(t *testing.T) {
	m := NewModel("testdata")
	fs, _ := m.Walk(false)
	m.ReplaceLocal(fs)

	if l1, l2 := len(m.local), len(fs); l1 != l2 {
		t.Errorf("Model len(local) incorrect (%d != %d)", l1, l2)
	}
	if l1, l2 := len(m.global), len(fs); l1 != l2 {
		t.Errorf("Model len(global) incorrect (%d != %d)", l1, l2)
	}
	if l1, l2 := len(m.need), 0; l1 != l2 {
		t.Errorf("Model len(need) incorrect (%d != %d)", l1, l2)
	}

	newFile := protocol.FileInfo{
		Name:     "new file",
		Modified: time.Now().Unix(),
		Blocks:   []protocol.BlockInfo{{100, []byte("some hash bytes")}},
	}
	m.Index("42", []protocol.FileInfo{newFile})

	if l1, l2 := len(m.local), len(fs); l1 != l2 {
		t.Errorf("Model len(local) incorrect (%d != %d)", l1, l2)
	}
	if l1, l2 := len(m.global), len(fs)+1; l1 != l2 {
		t.Errorf("Model len(global) incorrect (%d != %d)", l1, l2)
	}
	if l1, l2 := len(m.need), 1; l1 != l2 {
		t.Errorf("Model len(need) incorrect (%d != %d)", l1, l2)
	}

	m.Close("42", nil)

	if l1, l2 := len(m.local), len(fs); l1 != l2 {
		t.Errorf("Model len(local) incorrect (%d != %d)", l1, l2)
	}
	if l1, l2 := len(m.global), len(fs); l1 != l2 {
		t.Errorf("Model len(global) incorrect (%d != %d)", l1, l2)
	}
	if l1, l2 := len(m.need), 0; l1 != l2 {
		t.Errorf("Model len(need) incorrect (%d != %d)", l1, l2)
	}
}

func TestRequest(t *testing.T) {
	m := NewModel("testdata")
	fs, _ := m.Walk(false)
	m.ReplaceLocal(fs)

	bs, err := m.Request("some node", "foo", 0, 6, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Compare(bs, []byte("foobar")) != 0 {
		t.Errorf("Incorrect data from request: %q", string(bs))
	}

	bs, err = m.Request("some node", "../walk.go", 0, 6, nil)
	if err == nil {
		t.Error("Unexpected nil error on insecure file read")
	}
	if bs != nil {
		t.Errorf("Unexpected non nil data on insecure file read: %q", string(bs))
	}
}

func TestSuppression(t *testing.T) {
	var testdata = []struct {
		lastChange time.Time
		hold       int
		result     bool
	}{
		{time.Unix(0, 0), 0, false},                    // First change
		{time.Now().Add(-1 * time.Second), 0, true},    // Changed once one second ago, suppress
		{time.Now().Add(-119 * time.Second), 0, true},  // Changed once 119 seconds ago, suppress
		{time.Now().Add(-121 * time.Second), 0, false}, // Changed once 121 seconds ago, permit

		{time.Now().Add(-179 * time.Second), 1, true},  // Suppressed once 179 seconds ago, suppress again
		{time.Now().Add(-181 * time.Second), 1, false}, // Suppressed once 181 seconds ago, permit

		{time.Now().Add(-599 * time.Second), 99, true},  // Suppressed lots of times, last allowed 599 seconds ago, suppress again
		{time.Now().Add(-601 * time.Second), 99, false}, // Suppressed lots of times, last allowed 601 seconds ago, permit
	}

	for i, tc := range testdata {
		if shouldSuppressChange(tc.lastChange, tc.hold) != tc.result {
			t.Errorf("Incorrect result for test #%d: %v", i, tc)
		}
	}
}
