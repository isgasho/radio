package radio

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
)

const defaultBufSize = 4096

// NewReader initializes the RESP reader with given reader. In server mode,
// input data will be read line-by-line except in case of array of bulkstrings.
//
// Read https://redis.io/topics/protocol#sending-commands-to-a-redis-server for
// more information on how clients interact with server.
func NewReader(r io.Reader, isServer bool) *Reader {
	return NewReaderSize(r, isServer, defaultBufSize)
}

// NewReaderSize initializes the RESP reader with given buffer size.
// See NewReader for more information.
func NewReaderSize(r io.Reader, isServer bool, size int) *Reader {
	return &Reader{
		ir:       r,
		IsServer: isServer,
		buf:      make([]byte, size),
		sz:       size,
	}
}

// Reader implements server and client RESP protocol parser.
type Reader struct {
	ir      io.Reader
	start   int
	end     int
	buf     []byte
	sz      int
	inArray bool

	IsServer bool
}

// Read reads next RESP value from the stream.
func (rd *Reader) Read() (Value, error) {
	if err := rd.buffer(false); err != nil {
		return nil, err
	}
	prefix := rd.buf[rd.start]

	if rd.IsServer {
		if rd.inArray && prefix != '$' {
			return nil, fmt.Errorf("Protocol error: expecting '$', got '%c'", prefix)
		}

		if prefix != '*' && prefix != '$' {
			v, err := rd.readInline()
			if err != nil {
				return nil, err
			}
			return v, nil
		}
	}

	switch prefix {
	case '+':
		v, err := rd.readSimpleStr()
		if err != nil {
			return nil, err
		}
		return v, nil

	case '-':
		v, err := rd.readErrorStr()
		if err != nil {
			return nil, err
		}
		return v, nil

	case ':':
		v, err := rd.readInteger()
		if err != nil {
			return nil, err
		}

		return v, nil

	case '$':
		v, err := rd.readBulkStr()
		if err != nil {
			return nil, err
		}
		return v, nil

	case '*':
		v, err := rd.readArray()
		if err != nil {
			return nil, err
		}
		return v, nil
	}

	return nil, fmt.Errorf("bad prefix '%c'", prefix)
}

// Size returns the current buffer size and the minimum buffer size
// reader is configured with.
func (rd *Reader) Size() (minSize int, currentSize int) {
	return rd.sz, len(rd.buf)
}

func (rd *Reader) readSimpleStr() (SimpleStr, error) {
	rd.start++ // skip over '+'

	data, err := rd.readTillCRLF()
	if err != nil {
		return "", err
	}

	return SimpleStr(data), nil
}

func (rd *Reader) readErrorStr() (ErrorStr, error) {
	rd.start++ // skip over '-'

	data, err := rd.readTillCRLF()
	if err != nil {
		return "", err
	}

	return ErrorStr(data), nil
}

func (rd *Reader) readInteger() (Integer, error) {
	rd.start++ // skip over ':'

	n, err := rd.readNumber()
	return Integer(n), err
}

func (rd *Reader) readInline() (*Array, error) {
	data, err := rd.readTillCRLF()
	if err != nil {
		return nil, err
	}

	return &Array{
		Items: []Value{
			&BulkStr{
				Value: data,
			},
		},
	}, nil
}

func (rd *Reader) readBulkStr() (*BulkStr, error) {
	rd.start++ // skip over '$'

	size, err := rd.readNumber()
	if err != nil {
		if rd.IsServer && (err == errInvalidNumber || err == errNoNumber) {
			return nil, errors.New("Protocol error: invalid bulk length")
		}
		return nil, err
	}

	if size < 0 {
		if rd.IsServer {
			return nil, errors.New("Protocol error: invalid bulk length")
		}

		// -1 (negative size) means a null bulk string
		// Refer https://redis.io/topics/protocol#resp-bulk-strings
		return &BulkStr{}, nil
	}

	data, err := rd.readExactly(size)
	if err != nil {
		return nil, err
	}
	rd.start += 2 // skip over CRLF

	return &BulkStr{
		Value: data,
	}, nil
}

func (rd *Reader) readArray() (*Array, error) {
	rd.inArray = true
	defer func() {
		rd.inArray = false
	}()
	rd.start++ // skip over '+'

	size, err := rd.readNumber()
	if err != nil {
		if rd.IsServer && (err == errInvalidNumber || err == errNoNumber) {
			return nil, errors.New("Protocol error: invalid multibulk length")
		}

		return nil, err
	}

	if size < 0 {
		return &Array{}, nil
	}

	arr := &Array{}
	arr.Items = []Value{}

	for i := 0; i < size; i++ {
		item, err := rd.Read()
		if err != nil {
			return nil, err
		}

		arr.Items = append(arr.Items, item)
	}

	return arr, nil
}

func (rd *Reader) readExactly(n int) ([]byte, error) {
	for rd.end-rd.start < n {
		if err := rd.buffer(true); err != nil {
			return nil, err
		}
	}

	data := rd.buf[rd.start : rd.start+n]
	rd.start += n
	return data, nil
}

func (rd *Reader) readTillCRLF() ([]byte, error) {
	var crlf int
	for crlf = bytes.Index(rd.buf[rd.start:rd.end], []byte("\r\n")); crlf < 0; {
		if err := rd.buffer(true); err != nil {
			if err == io.EOF {
				break
			}

			return nil, err
		}
	}

	if crlf == 0 {
		return []byte(""), nil
	} else if crlf < 0 {
		data := rd.buf[rd.start:rd.end]
		rd.start = rd.end
		return data, io.EOF
	}

	data := make([]byte, crlf)
	copy(data, rd.buf[rd.start:rd.end])
	rd.start += crlf + 2
	return data, nil
}

func (rd *Reader) readNumber() (int, error) {
	data, err := rd.readTillCRLF()
	if err != nil {
		return 0, err
	}

	if len(data) == 0 {
		return 0, errNoNumber
	}

	return toInt(data)
}

func (rd *Reader) buffer(force bool) error {
	if !force && rd.end > rd.start {
		return nil // buffer already has some data.
	}

	if rd.end > 0 && rd.start >= rd.end {
		rd.start = 0
		rd.end = 0
	} else if rd.end == len(rd.buf) {
		rd.buf = append(rd.buf, make([]byte, rd.sz)...)
	}

	n, err := rd.ir.Read(rd.buf[rd.end:])
	if err != nil {
		return err
	}
	rd.end += n

	return nil
}

func toInt(data []byte) (int, error) {
	var d, sign int
	L := len(data)
	for i, b := range data {
		if i == 0 {
			if b == '-' {
				sign = -1
				continue
			}

			sign = 1
		}

		if b < '0' || b > '9' {
			return 0, errInvalidNumber
		}

		if b == '0' {
			continue
		}

		pos := int(math.Pow(10, float64(L-i-1)))
		d += int(b-'0') * pos
	}

	return sign * d, nil
}

var (
	errInvalidNumber = errors.New("invalid number format")
	errNoNumber      = errors.New("no number")
)
