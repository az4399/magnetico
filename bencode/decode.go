package bencode

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"runtime"
	"strconv"
	"sync"
)

// The default bencode string length limit. This is a poor attempt to prevent excessive memory
// allocation when parsing, but also leaves the window open to implement a better solution.
const DefaultDecodeMaxStrLen = 1<<27 - 1 // ~128MiB

type MaxStrLen = int64

type Decoder struct {
	// Maximum parsed bencode string length. Defaults to DefaultMaxStrLen if zero.
	MaxStrLen MaxStrLen

	r interface {
		io.ByteScanner
		io.Reader
	}
	// Sum of bytes used to Decode values.
	Offset int64
	buf    bytes.Buffer
}

// NewDecoder returns a new decoder that reads from r.
func (d *Decoder) Decode(v interface{}) (err error) {
	defer func() {
		if err != nil {
			return
		}
		r := recover()
		if r == nil {
			return
		}
		_, ok := r.(runtime.Error)
		if ok {
			panic(r)
		}
		if err, ok = r.(error); !ok {
			panic(r)
		}
		// Errors thrown from deeper in parsing are unexpected. At value boundaries, errors should
		// be returned directly (at least until all the panic nonsense is removed entirely).
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
	}()

	pv := reflect.ValueOf(v)
	if pv.Kind() != reflect.Ptr || pv.IsNil() {
		return &UnmarshalInvalidArgError{reflect.TypeOf(v)}
	}

	ok, err := d.parseValue(pv.Elem())
	if err != nil {
		return
	}
	if !ok {
		d.throwSyntaxError(d.Offset-1, errors.New("unexpected 'e'"))
	}
	return
}

// Check for EOF in the decoder input stream. Used to assert the input ends on a clean message
// boundary.
func (d *Decoder) ReadEOF() error {
	_, err := d.r.ReadByte()
	if err == nil {
		err := d.r.UnreadByte()
		if err != nil {
			panic(err)
		}
		return err
	}
	if err == io.EOF {
		return nil
	}
	return fmt.Errorf("expected EOF, got %w", err)
}

func checkForUnexpectedEOF(err error, offset int64) {
	if err == io.EOF {
		panic(&SyntaxError{
			Offset: offset,
			What:   io.ErrUnexpectedEOF,
		})
	}
}

func (d *Decoder) readByte() byte {
	b, err := d.r.ReadByte()
	if err != nil {
		checkForUnexpectedEOF(err, d.Offset)
		panic(err)
	}

	d.Offset++
	return b
}

// reads data writing it to 'd.buf' until 'sep' byte is encountered, 'sep' byte
// is consumed, but not included into the 'd.buf'
func (d *Decoder) readUntil(sep byte) {
	for {
		b := d.readByte()
		if b == sep {
			return
		}
		d.buf.WriteByte(b)
	}
}

func checkForIntParseError(err error, offset int64) {
	if err != nil {
		panic(&SyntaxError{
			Offset: offset,
			What:   err,
		})
	}
}

func (d *Decoder) throwSyntaxError(offset int64, err error) {
	panic(&SyntaxError{
		Offset: offset,
		What:   err,
	})
}

// Assume the 'i' is already consumed. Read and validate the rest of an int into the buffer.
func (d *Decoder) readInt() error {
	// start := d.Offset - 1
	d.readUntil('e')
	return d.checkBufferedInt()
}

// called when 'i' was consumed, for the integer type in v.
func (d *Decoder) parseInt(v reflect.Value) error {
	start := d.Offset - 1

	if err := d.readInt(); err != nil {
		return err
	}
	s := d.buf.String()

	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		checkForIntParseError(err, start)

		if v.OverflowInt(n) {
			return &UnmarshalTypeError{
				BencodeTypeName:     "int",
				UnmarshalTargetType: v.Type(),
			}
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, 64)
		checkForIntParseError(err, start)

		if v.OverflowUint(n) {
			return &UnmarshalTypeError{
				BencodeTypeName:     "int",
				UnmarshalTargetType: v.Type(),
			}
		}
		v.SetUint(n)
	case reflect.Bool:
		v.SetBool(s != "0")
	default:
		return &UnmarshalTypeError{
			BencodeTypeName:     "int",
			UnmarshalTargetType: v.Type(),
		}
	}
	d.buf.Reset()
	return nil
}

func (d *Decoder) checkBufferedInt() error {
	b := d.buf.Bytes()
	if len(b) <= 1 {
		return nil
	}
	if b[0] == '-' {
		b = b[1:]
	}
	if b[0] < '1' || b[0] > '9' {
		return errors.New("invalid leading digit")
	}
	return nil
}

func (d *Decoder) parseStringLength() (int, error) {
	// We should have already consumed the first byte of the length into the Decoder buf.
	start := d.Offset - 1
	d.readUntil(':')
	if err := d.checkBufferedInt(); err != nil {
		return 0, err
	}
	// Really the limit should be the uint size for the platform. But we can't pass in an allocator,
	// or limit total memory use in Go, the best we might hope to do is limit the size of a single
	// decoded value (by reading it in in-place and then operating on a view).
	length, err := strconv.ParseInt(d.buf.String(), 10, 0)
	checkForIntParseError(err, start)
	if int64(length) > d.getMaxStrLen() {
		err = fmt.Errorf("parsed string length %v exceeds limit (%v)", length, DefaultDecodeMaxStrLen)
	}
	d.buf.Reset()
	return int(length), err
}

func (d *Decoder) parseString(v reflect.Value) error {
	length, err := d.parseStringLength()
	if err != nil {
		return err
	}
	defer d.buf.Reset()
	read := func(b []byte) {
		n, err := io.ReadFull(d.r, b)
		d.Offset += int64(n)
		if err != nil {
			checkForUnexpectedEOF(err, d.Offset)
			panic(&SyntaxError{
				Offset: d.Offset,
				What:   errors.New("unexpected I/O error: " + err.Error()),
			})
		}
	}

	switch v.Kind() {
	case reflect.String:
		b := make([]byte, length)
		read(b)
		v.SetString(string(b))
		return nil
	case reflect.Slice:
		if v.Type().Elem().Kind() != reflect.Uint8 {
			break
		}
		b := make([]byte, length)
		read(b)
		v.SetBytes(b)
		return nil
	case reflect.Array:
		if v.Type().Elem().Kind() != reflect.Uint8 {
			break
		}
		d.buf.Grow(length)
		b := d.buf.Bytes()[:length]
		read(b)
		reflect.Copy(v, reflect.ValueOf(b))
		return nil
	case reflect.Bool:
		d.buf.Grow(length)
		b := d.buf.Bytes()[:length]
		read(b)
		x, err := strconv.ParseBool(string(b))
		if err != nil {
			x = length != 0
		}
		v.SetBool(x)
		return nil
	}
	// Can't move this into default clause because some cases above fail through to here after
	// additional checks.
	d.buf.Grow(length)
	read(d.buf.Bytes()[:length])
	// I believe we return here to support "ignore_unmarshal_type_error".
	return &UnmarshalTypeError{
		BencodeTypeName:     "string",
		UnmarshalTargetType: v.Type(),
	}
}

// Info for parsing a dict value.
type dictField struct {
	Type reflect.Type
	Get  func(value reflect.Value) func(reflect.Value)
	Tags tag
}

// Returns specifics for parsing a dict field value.
func getDictField(dict reflect.Type, key reflect.Value) (_ dictField, err error) {
	// get valuev as a map value or as a struct field
	switch k := dict.Kind(); k {
	case reflect.Map:
		return dictField{
			Type: dict.Elem(),
			Get: func(mapValue reflect.Value) func(reflect.Value) {
				return func(value reflect.Value) {
					if mapValue.IsNil() {
						mapValue.Set(reflect.MakeMap(dict))
					}
					// Assigns the value into the map.
					mapValue.SetMapIndex(key, value)
				}
			},
		}, nil
	case reflect.Struct:
		if key.Kind() != reflect.String {
			// This doesn't make sense for structs. They have to use strings. If they didn't they
			// should at least have things that convert to strings trivially and somehow much the
			// bencode tag.
			panic(key)
		}
		return getStructFieldForKey(dict, key.String()), nil
	default:
		err = fmt.Errorf("can't assign bencode dict items into a %v", k)
		return
	}
}

var (
	structFieldsMu sync.Mutex
	structFields   = map[reflect.Type]map[string]dictField{}
)

func parseStructFields(struct_ reflect.Type, each func(key string, df dictField)) {
	for _i, n := 0, struct_.NumField(); _i < n; _i++ {
		i := _i
		f := struct_.Field(i)
		if f.Anonymous {
			t := f.Type
			if t.Kind() == reflect.Ptr {
				t = t.Elem()
			}
			parseStructFields(t, func(key string, df dictField) {
				innerGet := df.Get
				df.Get = func(value reflect.Value) func(reflect.Value) {
					anonPtr := value.Field(i)
					if anonPtr.Kind() == reflect.Ptr && anonPtr.IsNil() {
						anonPtr.Set(reflect.New(f.Type.Elem()))
						anonPtr = anonPtr.Elem()
					}
					return innerGet(anonPtr)
				}
				each(key, df)
			})
			continue
		}
		tagStr := f.Tag.Get("bencode")
		if tagStr == "-" {
			continue
		}
		tag := parseTag(tagStr)
		key := tag.Key()
		if key == "" {
			key = f.Name
		}
		each(key, dictField{f.Type, func(value reflect.Value) func(reflect.Value) {
			return value.Field(i).Set
		}, tag})
	}
}

func saveStructFields(struct_ reflect.Type) {
	m := make(map[string]dictField)
	parseStructFields(struct_, func(key string, sf dictField) {
		m[key] = sf
	})
	structFields[struct_] = m
}

func getStructFieldForKey(struct_ reflect.Type, key string) (f dictField) {
	structFieldsMu.Lock()
	if _, ok := structFields[struct_]; !ok {
		saveStructFields(struct_)
	}
	f, ok := structFields[struct_][key]
	structFieldsMu.Unlock()
	if !ok {
		var discard interface{}
		return dictField{
			Type: reflect.TypeOf(discard),
			Get:  func(reflect.Value) func(reflect.Value) { return func(reflect.Value) {} },
			Tags: nil,
		}
	}
	return
}

var structKeyType = reflect.TypeFor[string]()

func keyType(v reflect.Value) reflect.Type {
	switch v.Kind() {
	case reflect.Map:
		return v.Type().Key()
	case reflect.Struct:
		return structKeyType
	default:
		return nil
	}
}

func (d *Decoder) parseDict(v reflect.Value) error {
	// At this point 'd' byte was consumed, now read key/value pairs.

	// The key type does not need to be a string for maps.
	keyType := keyType(v)
	if keyType == nil {
		return fmt.Errorf("cannot parse dicts into %v", v.Type())
	}
	for {
		keyValue := reflect.New(keyType).Elem()
		ok, err := d.parseValue(keyValue)
		if err != nil {
			return fmt.Errorf("error parsing dict key: %w", err)
		}
		if !ok {
			return nil
		}

		df, err := getDictField(v.Type(), keyValue)
		if err != nil {
			return fmt.Errorf("parsing bencode dict into %v: %w", v.Type(), err)
		}

		// now we need to actually parse it
		if df.Type == nil {
			// Discard the value, there's nowhere to put it.
			var if_ interface{}
			if_, ok = d.parseValueInterface()
			if if_ == nil {
				return fmt.Errorf("error parsing value for key %q", keyValue)
			}
			if !ok {
				return fmt.Errorf("missing value for key %q", keyValue)
			}
			continue
		}
		setValue := reflect.New(df.Type).Elem()
		ok, err = d.parseValue(setValue)
		if err != nil {
			var target *UnmarshalTypeError
			if !errors.As(err, &target) || !df.Tags.IgnoreUnmarshalTypeError() {
				return fmt.Errorf("parsing value for key %q: %w", keyValue, err)
			}
		}
		if !ok {
			return fmt.Errorf("missing value for key %q", keyValue)
		}
		df.Get(v)(setValue)
	}
}

func (d *Decoder) parseList(v reflect.Value) error {
	switch v.Kind() {
	default:
		// If the list is a singleton of the expected type, use that value.
		l := reflect.New(reflect.SliceOf(v.Type()))
		if err := d.parseList(l.Elem()); err != nil {
			return err
		}
		if l.Elem().Len() != 1 {
			return &UnmarshalTypeError{
				BencodeTypeName:     "list",
				UnmarshalTargetType: v.Type(),
			}
		}
		v.Set(l.Elem().Index(0))
		return nil
	case reflect.Array, reflect.Slice:
		// We can work with this. Normal case, fallthrough.
	}

	i := 0
	for ; ; i++ {
		if v.Kind() == reflect.Slice && i >= v.Len() {
			v.Set(reflect.Append(v, reflect.Zero(v.Type().Elem())))
		}

		if i < v.Len() {
			ok, err := d.parseValue(v.Index(i))
			if err != nil {
				return err
			}
			if !ok {
				break
			}
		} else {
			_, ok := d.parseValueInterface()
			if !ok {
				break
			}
		}
	}

	if i < v.Len() {
		if v.Kind() == reflect.Array {
			z := reflect.Zero(v.Type().Elem())
			for n := v.Len(); i < n; i++ {
				v.Index(i).Set(z)
			}
		} else {
			v.SetLen(i)
		}
	}

	if i == 0 && v.Kind() == reflect.Slice {
		v.Set(reflect.MakeSlice(v.Type(), 0, 0))
	}
	return nil
}

func (d *Decoder) readOneValue() bool {
	b, err := d.r.ReadByte()
	if err != nil {
		panic(err)
	}
	if b == 'e' {
		if err := d.r.UnreadByte(); err != nil {
			panic(err)
		}
		return false
	} else {
		d.Offset++
		d.buf.WriteByte(b)
	}

	switch b {
	case 'd', 'l':
		// read until there is nothing to read
		for d.readOneValue() {
		}
		// consume 'e' as well
		b = d.readByte()
		d.buf.WriteByte(b)
	case 'i':
		d.readUntil('e')
		d.buf.WriteString("e")
	default:
		if b >= '0' && b <= '9' {
			start := d.buf.Len() - 1
			d.readUntil(':')
			length, err := strconv.ParseInt(d.buf.String()[start:], 10, 64)
			checkForIntParseError(err, d.Offset-1)

			d.buf.WriteString(":")
			n, err := io.CopyN(&d.buf, d.r, length)
			d.Offset += n
			if err != nil {
				checkForUnexpectedEOF(err, d.Offset)
				panic(&SyntaxError{
					Offset: d.Offset,
					What:   errors.New("unexpected I/O error: " + err.Error()),
				})
			}
			break
		}

		d.raiseUnknownValueType(b, d.Offset-1)
	}

	return true
}

func (d *Decoder) parseUnmarshaler(v reflect.Value) bool {
	if !v.Type().Implements(reflect.TypeOf((*Unmarshaler)(nil)).Elem()) {
		if v.Addr().Type().Implements(reflect.TypeOf((*Unmarshaler)(nil)).Elem()) {
			v = v.Addr()
		} else {
			return false
		}
	}
	d.buf.Reset()
	if !d.readOneValue() {
		return false
	}
	m := v.Interface().(Unmarshaler)
	err := m.UnmarshalBencode(d.buf.Bytes())
	if err != nil {
		panic(&UnmarshalerError{v.Type(), err})
	}
	return true
}

// Returns true if there was a value and it's now stored in 'v'. Otherwise, there was an end symbol
// ("e") and no value was stored.
func (d *Decoder) parseValue(v reflect.Value) (bool, error) {
	// we support one level of indirection at the moment
	if v.Kind() == reflect.Ptr {
		// if the pointer is nil, allocate a new element of the type it
		// points to
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		v = v.Elem()
	}

	if d.parseUnmarshaler(v) {
		return true, nil
	}

	// common case: interface{}
	if v.Kind() == reflect.Interface && v.NumMethod() == 0 {
		iface, _ := d.parseValueInterface()
		v.Set(reflect.ValueOf(iface))
		return true, nil
	}

	b, err := d.r.ReadByte()
	if err != nil {
		return false, err
	}
	d.Offset++

	switch b {
	case 'e':
		return false, nil
	case 'd':
		return true, d.parseDict(v)
	case 'l':
		return true, d.parseList(v)
	case 'i':
		return true, d.parseInt(v)
	default:
		if b >= '0' && b <= '9' {
			// It's a string.
			d.buf.Reset()
			// Write the first digit of the length to the buffer.
			d.buf.WriteByte(b)
			return true, d.parseString(v)
		}

		d.raiseUnknownValueType(b, d.Offset-1)
	}
	panic("unreachable")
}

// An unknown bencode type character was encountered.
func (d *Decoder) raiseUnknownValueType(b byte, offset int64) {
	panic(&SyntaxError{
		Offset: offset,
		What:   fmt.Errorf("unknown value type %+q", b),
	})
}

func (d *Decoder) parseValueInterface() (interface{}, bool) {
	b, err := d.r.ReadByte()
	if err != nil {
		panic(err)
	}
	d.Offset++

	switch b {
	case 'e':
		return nil, false
	case 'd':
		return d.parseDictInterface(), true
	case 'l':
		return d.parseListInterface(), true
	case 'i':
		return d.parseIntInterface(), true
	default:
		if b >= '0' && b <= '9' {
			// string
			// append first digit of the length to the buffer
			d.buf.WriteByte(b)
			return d.parseStringInterface(), true
		}

		d.raiseUnknownValueType(b, d.Offset-1)
		panic("unreachable")
	}
}

// Called after 'i', for an arbitrary integer size.
func (d *Decoder) parseIntInterface() (ret interface{}) {
	start := d.Offset - 1

	if err := d.readInt(); err != nil {
		panic(err)
	}
	n, err := strconv.ParseInt(d.buf.String(), 10, 64)
	if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
		i := new(big.Int)
		_, ok := i.SetString(d.buf.String(), 10)
		if !ok {
			panic(&SyntaxError{
				Offset: start,
				What:   errors.New("failed to parse integer"),
			})
		}
		ret = i
	} else {
		checkForIntParseError(err, start)
		ret = n
	}

	d.buf.Reset()
	return
}

func (d *Decoder) readBytes(length int) []byte {
	b, err := io.ReadAll(io.LimitReader(d.r, int64(length)))
	if err != nil {
		panic(err)
	}
	if len(b) != length {
		panic(fmt.Errorf("read %v bytes expected %v", len(b), length))
	}
	return b
}

func (d *Decoder) parseStringInterface() string {
	length, err := d.parseStringLength()
	if err != nil {
		panic(err)
	}
	b := d.readBytes(int(length))
	d.Offset += int64(len(b))
	return string(b)
}

func (d *Decoder) parseDictInterface() interface{} {
	dict := make(map[string]interface{})
	var lastKey string
	lastKeyOk := false
	for {
		start := d.Offset
		keyi, ok := d.parseValueInterface()
		if !ok {
			break
		}

		key, ok := keyi.(string)
		if !ok {
			panic(&SyntaxError{
				Offset: d.Offset,
				What:   errors.New("non-string key in a dict"),
			})
		}
		if lastKeyOk && key <= lastKey {
			d.throwSyntaxError(start, fmt.Errorf("dict keys unsorted: %q <= %q", key, lastKey))
		}
		start = d.Offset
		valuei, ok := d.parseValueInterface()
		if !ok {
			d.throwSyntaxError(start, fmt.Errorf("dict elem missing value [key=%v]", key))
		}

		lastKey = key
		lastKeyOk = true
		dict[key] = valuei
	}
	return dict
}

func (d *Decoder) parseListInterface() (list []interface{}) {
	list = []interface{}{}
	valuei, ok := d.parseValueInterface()
	for ok {
		list = append(list, valuei)
		valuei, ok = d.parseValueInterface()
	}
	return
}

func (d *Decoder) getMaxStrLen() int64 {
	if d.MaxStrLen == 0 {
		return DefaultDecodeMaxStrLen
	}
	return d.MaxStrLen
}
