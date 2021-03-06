// The MIT License (MIT)

// Copyright (c) 2016 Jerry Bai

// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:

// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package mysql

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"

	"github.com/berkaroad/saashard/errors"
)

// WriteStmtPrepareResponse write stmt prepare
func (p *PacketIO) WriteStmtPrepareResponse(capability uint32, status uint16, s *Stmt) error {
	var err error
	data := make([]byte, 4, 128)
	total := make([]byte, 0, 1024)
	//status ok
	data = append(data, 0)
	//stmt id
	data = append(data, Uint32ToBytes(s.ID)...)
	//number columns
	data = append(data, Uint16ToBytes(uint16(s.ColumnNum))...)
	//number params
	data = append(data, Uint16ToBytes(uint16(s.ParamNum))...)
	//filter [00]
	data = append(data, 0)
	//warning count
	data = append(data, 0, 0)

	total, err = p.WritePacketBatch(total, data, false)
	if err != nil {
		return err
	}

	if s.ParamNum > 0 {
		for i := 0; i < s.ParamNum; i++ {
			data = data[0:4]
			data = append(data, s.Params[i].Dump()...)

			total, err = p.WritePacketBatch(total, data, false)
			if err != nil {
				return err
			}
		}

		total, err = p.WriteEOFBatch(total, capability, status, false)
		if err != nil {
			return err
		}
	}

	if s.ColumnNum > 0 {
		for i := 0; i < s.ColumnNum; i++ {
			data = data[0:4]
			data = append(data, s.Columns[i].Dump()...)

			total, err = p.WritePacketBatch(total, data, false)
			if err != nil {
				return err
			}
		}

		total, err = p.WriteEOFBatch(total, capability, status, false)
		if err != nil {
			return err
		}

	}
	total, err = p.WritePacketBatch(total, nil, true)
	total = nil
	if err != nil {
		return err
	}
	return nil
}

// ReadStmtExecuteRequest read from stmt execute request
func (p *PacketIO) ReadStmtExecuteRequest(data []byte, findStmtByID func(id uint32) *Stmt) (*Stmt, error) {
	if len(data) < 9 {
		return nil, errors.ErrMalformPacket
	}
	PrintPacketData("ReadStmtExecuteRequest", data)

	pos := 0
	id := binary.LittleEndian.Uint32(data[0:4])
	pos += 4

	s := findStmtByID(id)
	if s == nil {
		return nil, NewDefaultError(ER_UNKNOWN_STMT_HANDLER,
			strconv.FormatUint(uint64(id), 10), "stmt_execute")
	}

	flag := data[pos]
	pos++
	//now we only support CURSOR_TYPE_NO_CURSOR flag
	if flag != 0 {
		return nil, NewError(ER_UNKNOWN_ERROR, fmt.Sprintf("unsupported flag %d", flag))
	}

	//skip iteration-count, always 1
	pos += 4

	var nullBitmaps []byte
	var paramTypes []byte
	var paramValues []byte

	paramNum := s.ParamNum

	if paramNum > 0 {
		nullBitmapLen := (s.ParamNum + 7) >> 3
		if len(data) < (pos + nullBitmapLen + 1) {
			return nil, errors.ErrMalformPacket
		}
		nullBitmaps = data[pos : pos+nullBitmapLen]
		pos += nullBitmapLen

		//new param bound flag
		if data[pos] == 1 {
			pos++
			if len(data) < (pos + (paramNum << 1)) {
				return nil, errors.ErrMalformPacket
			}

			paramTypes = data[pos : pos+(paramNum<<1)]
			pos += (paramNum << 1)

			paramValues = data[pos:]

			if err := p.bindStmtArgs(s, nullBitmaps, paramTypes, paramValues); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

func (p *PacketIO) bindStmtArgs(s *Stmt, nullBitmap, paramTypes, paramValues []byte) error {
	args := s.Args

	pos := 0

	var v []byte
	var n = 0
	var isNull bool
	var err error

	for i := 0; i < s.ParamNum; i++ {
		if nullBitmap[i>>3]&(1<<(uint(i)%8)) > 0 {
			args[i] = nil
			continue
		}

		tp := paramTypes[i<<1]
		isUnsigned := (paramTypes[(i<<1)+1] & 0x80) > 0

		switch tp {
		case MYSQL_TYPE_NULL:
			args[i] = nil
			continue

		case MYSQL_TYPE_TINY:
			if len(paramValues) < (pos + 1) {
				return errors.ErrMalformPacket
			}

			if isUnsigned {
				args[i] = uint8(paramValues[pos])
			} else {
				args[i] = int8(paramValues[pos])
			}

			pos++
			continue

		case MYSQL_TYPE_SHORT, MYSQL_TYPE_YEAR:
			if len(paramValues) < (pos + 2) {
				return errors.ErrMalformPacket
			}

			if isUnsigned {
				args[i] = uint16(binary.LittleEndian.Uint16(paramValues[pos : pos+2]))
			} else {
				args[i] = int16((binary.LittleEndian.Uint16(paramValues[pos : pos+2])))
			}
			pos += 2
			continue

		case MYSQL_TYPE_INT24, MYSQL_TYPE_LONG:
			if len(paramValues) < (pos + 4) {
				return errors.ErrMalformPacket
			}

			if isUnsigned {
				args[i] = binary.LittleEndian.Uint32(paramValues[pos : pos+4])
			} else {
				args[i] = int32(binary.LittleEndian.Uint32(paramValues[pos : pos+4]))
			}
			pos += 4
			continue

		case MYSQL_TYPE_LONGLONG:
			if len(paramValues) < (pos + 8) {
				return errors.ErrMalformPacket
			}

			if isUnsigned {
				args[i] = binary.LittleEndian.Uint64(paramValues[pos : pos+8])
			} else {
				args[i] = int64(binary.LittleEndian.Uint64(paramValues[pos : pos+8]))
			}
			pos += 8
			continue

		case MYSQL_TYPE_FLOAT:
			if len(paramValues) < (pos + 4) {
				return errors.ErrMalformPacket
			}

			args[i] = float32(math.Float32frombits(binary.LittleEndian.Uint32(paramValues[pos : pos+4])))
			pos += 4
			continue

		case MYSQL_TYPE_DOUBLE:
			if len(paramValues) < (pos + 8) {
				return errors.ErrMalformPacket
			}

			args[i] = math.Float64frombits(binary.LittleEndian.Uint64(paramValues[pos : pos+8]))
			pos += 8
			continue

		case MYSQL_TYPE_DECIMAL, MYSQL_TYPE_NEWDECIMAL, MYSQL_TYPE_VARCHAR,
			MYSQL_TYPE_BIT, MYSQL_TYPE_ENUM, MYSQL_TYPE_SET, MYSQL_TYPE_TINY_BLOB,
			MYSQL_TYPE_MEDIUM_BLOB, MYSQL_TYPE_LONG_BLOB, MYSQL_TYPE_BLOB,
			MYSQL_TYPE_VAR_STRING, MYSQL_TYPE_STRING, MYSQL_TYPE_GEOMETRY,
			MYSQL_TYPE_DATE, MYSQL_TYPE_NEWDATE,
			MYSQL_TYPE_TIMESTAMP, MYSQL_TYPE_DATETIME, MYSQL_TYPE_TIME:
			if len(paramValues) < (pos + 1) {
				return errors.ErrMalformPacket
			}

			v, isNull, n, err = LenencStrToString(paramValues[pos:])
			pos += n
			if err != nil {
				return err
			}

			if !isNull {
				args[i] = v
				continue
			} else {
				args[i] = nil
				continue
			}
		default:
			return fmt.Errorf("Stmt Unknown FieldType %d", tp)
		}
	}
	return nil
}
