package protocol

import (
	"cmp"
	"maps"
	"slices"
	"strings"
)

// sharedIDLimit is the highest QuasarMessage field number in the Max schema
// dump. Raw frames captured on Station 2 with ids in this range decode into the
// Max message types, so the newer Buildroot build reuses that numbering and
// only wraps the body in fields 252/253; its own ids start above this limit and
// stay unnamed until a dump identifies them.
const sharedIDLimit = 157

// Message describes a QuasarMessage field.
type Message struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Kind string `json:"kind"`
}

// Table provides read-only access to the known messages for one firmware family.
type Table struct {
	name string
	byID map[int]Message
	byNm map[string]Message
}

var tables = map[string]*Table{"max": newTable("max"), "linux": newTable("linux")}

func newTable(name string) *Table {
	return &Table{name: name, byID: make(map[int]Message), byNm: make(map[string]Message)}
}

func (t *Table) add(m Message) { t.byID[m.ID] = m; t.byNm[strings.ToLower(m.Name)] = m }

// MessageTable returns the known messages for the "max" or "linux" layout.
func MessageTable(build string) (*Table, bool) { t, ok := tables[build]; return t, ok }

// Build returns the firmware family of the table.
func (t *Table) Build() string { return t.name }

// ByID looks up a QuasarMessage field by number.
func (t *Table) ByID(id int) (Message, bool) { m, ok := t.byID[id]; return m, ok }

// ByName looks up a case-insensitive protobuf field name.
func (t *Table) ByName(name string) (Message, bool) {
	m, ok := t.byNm[strings.ToLower(name)]
	return m, ok
}

// Messages returns a copy of the entries ordered by field number.
func (t *Table) Messages() []Message {
	entries := slices.Collect(maps.Values(t.byID))
	slices.SortFunc(entries, func(a, b Message) int { return cmp.Compare(a.ID, b.ID) })
	return entries
}

// WireType maps the message kind to the protobuf wire type of its value, used when
// sending a typed message to the Max build.
func (m Message) WireType() uint64 {
	switch m.Kind {
	case "int32", "int64", "uint32", "uint64", "sint32", "sint64", "bool", "enum":
		return 0
	case "fixed64", "sfixed64", "double":
		return 1
	case "fixed32", "sfixed32", "float":
		return 5
	}
	return 2
}
