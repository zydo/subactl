package bench

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// A minimal Parquet writer: required columns of INT32, INT64, DOUBLE and
// UTF-8 strings, PLAIN encoding, no compression, one row group, one data
// page per column. It is all the experiment's raw series need, and it
// keeps the project free of third-party dependencies. Any Parquet reader
// can open the result.

// Parquet physical types.
const (
	pqInt32     = 1
	pqInt64     = 2
	pqDouble    = 5
	pqByteArray = 6
)

// Column is one column of a table: exactly one of the slices is set.
type Column struct {
	Name    string
	Int32s  []int32
	Int64s  []int64
	Doubles []float64
	Strings []string
}

func (c Column) kind() (typ int32, rows int) {
	switch {
	case c.Int32s != nil:
		return pqInt32, len(c.Int32s)
	case c.Int64s != nil:
		return pqInt64, len(c.Int64s)
	case c.Doubles != nil:
		return pqDouble, len(c.Doubles)
	}
	return pqByteArray, len(c.Strings)
}

// plain encodes the column's values in Parquet's PLAIN encoding.
func (c Column) plain() []byte {
	var b bytes.Buffer
	switch {
	case c.Int32s != nil:
		for _, v := range c.Int32s {
			_ = binary.Write(&b, binary.LittleEndian, v)
		}
	case c.Int64s != nil:
		for _, v := range c.Int64s {
			_ = binary.Write(&b, binary.LittleEndian, v)
		}
	case c.Doubles != nil:
		for _, v := range c.Doubles {
			_ = binary.Write(&b, binary.LittleEndian, math.Float64bits(v))
		}
	default:
		for _, v := range c.Strings {
			_ = binary.Write(&b, binary.LittleEndian, uint32(len(v)))
			b.WriteString(v)
		}
	}
	return b.Bytes()
}

// thrift is a writer for Thrift's compact protocol, which Parquet uses for
// its page headers and footer.
type thrift struct {
	bytes.Buffer
	last []int16 // the previous field id, per nested struct
}

// Thrift compact type codes.
const (
	tI32    = 5
	tI64    = 6
	tBinary = 8
	tList   = 9
	tStruct = 12
)

func (t *thrift) varint(v uint64) {
	for v >= 0x80 {
		t.WriteByte(byte(v) | 0x80)
		v >>= 7
	}
	t.WriteByte(byte(v))
}

func (t *thrift) zigzag(v int64) { t.varint(uint64(v<<1) ^ uint64(v>>63)) }

func (t *thrift) begin() { t.last = append(t.last, 0) }

func (t *thrift) end() {
	t.WriteByte(0)
	t.last = t.last[:len(t.last)-1]
}

// field writes a field header. Ids must be written in increasing order.
func (t *thrift) field(id int16, typ byte) {
	top := len(t.last) - 1
	if delta := id - t.last[top]; delta > 0 && delta <= 15 {
		t.WriteByte(byte(delta)<<4 | typ)
	} else {
		t.WriteByte(typ)
		t.zigzag(int64(id))
	}
	t.last[top] = id
}

func (t *thrift) i32(id int16, v int32) { t.field(id, tI32); t.zigzag(int64(v)) }
func (t *thrift) i64(id int16, v int64) { t.field(id, tI64); t.zigzag(v) }

func (t *thrift) str(s string) { t.varint(uint64(len(s))); t.WriteString(s) }

func (t *thrift) string(id int16, s string) { t.field(id, tBinary); t.str(s) }

func (t *thrift) list(id int16, elem byte, n int) {
	t.field(id, tList)
	if n < 15 {
		t.WriteByte(byte(n)<<4 | elem)
	} else {
		t.WriteByte(0xf0 | elem)
		t.varint(uint64(n))
	}
}

// WriteParquet writes the columns as one Parquet file.
func WriteParquet(w io.Writer, cols []Column) error {
	if len(cols) == 0 {
		return fmt.Errorf("parquet: no columns")
	}
	_, rows := cols[0].kind()
	for _, c := range cols {
		if _, n := c.kind(); n != rows {
			return fmt.Errorf("parquet: column %s has %d rows, want %d", c.Name, n, rows)
		}
	}
	var file bytes.Buffer
	file.WriteString("PAR1")
	type chunk struct {
		offset, size int64
	}
	chunks := make([]chunk, len(cols))
	for i, c := range cols {
		data := c.plain()
		// PageHeader: a version 1 data page. Required columns have no
		// repetition or definition levels, so the page is the values alone.
		var h thrift
		h.begin()
		h.i32(1, 0) // type: DATA_PAGE
		h.i32(2, int32(len(data)))
		h.i32(3, int32(len(data)))
		h.field(5, tStruct) // data_page_header
		h.begin()
		h.i32(1, int32(rows))
		h.i32(2, 0) // encoding: PLAIN
		h.i32(3, 3) // definition levels: RLE (none are stored)
		h.i32(4, 3) // repetition levels: RLE (none are stored)
		h.end()
		h.end()
		chunks[i] = chunk{offset: int64(file.Len()), size: int64(h.Len() + len(data))}
		file.Write(h.Bytes())
		file.Write(data)
	}

	// FileMetaData.
	var m thrift
	m.begin()
	m.i32(1, 1) // version
	m.list(2, tStruct, len(cols)+1)
	m.begin() // the root of the schema
	m.string(4, "schema")
	m.i32(5, int32(len(cols)))
	m.end()
	for _, c := range cols {
		typ, _ := c.kind()
		m.begin()
		m.i32(1, typ)
		m.i32(3, 0) // repetition: REQUIRED
		m.string(4, c.Name)
		if typ == pqByteArray {
			m.i32(6, 0) // converted type: UTF8
		}
		m.end()
	}
	m.i64(3, int64(rows))
	m.list(4, tStruct, 1)
	m.begin() // the one RowGroup
	m.list(1, tStruct, len(cols))
	var total int64
	for i, c := range cols {
		typ, _ := c.kind()
		total += chunks[i].size
		m.begin() // ColumnChunk
		m.i64(2, chunks[i].offset)
		m.field(3, tStruct) // ColumnMetaData
		m.begin()
		m.i32(1, typ)
		m.list(2, tI32, 1)
		m.zigzag(0) // encodings: PLAIN
		m.list(3, tBinary, 1)
		m.str(c.Name)
		m.i32(4, 0) // codec: UNCOMPRESSED
		m.i64(5, int64(rows))
		m.i64(6, chunks[i].size)
		m.i64(7, chunks[i].size)
		m.i64(9, chunks[i].offset)
		m.end()
		m.end()
	}
	m.i64(2, total)
	m.i64(3, int64(rows))
	m.end()
	m.string(6, "subactl bench")
	m.end()

	file.Write(m.Bytes())
	_ = binary.Write(&file, binary.LittleEndian, uint32(m.Len()))
	file.WriteString("PAR1")
	_, err := w.Write(file.Bytes())
	return err
}
