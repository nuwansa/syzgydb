/*
Span File Format Grammar:

SpanFile ::= Span*
Span ::= MagicNumber (4)
         SpanLength (4)
         SequenceNumber (7code)
         RecordIDLength (7code)
         RecordID (...bytes)
         DataStreamCount (byte)
         DataStream*
         Padding (varies)
         Checksum (4 bytes CRC)

DataStream ::= StreamID (1)
  StreamLength (7code)
  StreamData (...bytes)

Padding is placed in a span if it is placed before another record,
and there is not enough space to fit in at leeast an empty span.
(4+1+1+1+1+4 = 12 bytes)
*/

package syzgydb

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"os"
	"sync"

	"github.com/edsrzf/mmap-go"
)

const (
	activeMagic = 0x5350414E // 'SPAN'
	freeMagic   = 0x46524545 // 'FREE'
)

const minSpanLength = 15

type DataStream struct {
	StreamID uint8
	Data     []byte
}

func (db *SpanFile) Close() error {
	db.fileMutex.Lock()
	defer db.fileMutex.Unlock()

	if db.mmapData != nil {
		err := msync(db.mmapData)
		if err != nil {
			return err
		}
		err = db.mmapData.Unmap()
		if err != nil {
			return err
		}
		db.mmapData = nil
	}
	if db.file != nil {
		err := db.file.Close()
		if err != nil {
			return err
		}
		db.file = nil
	}
	return nil
}

type Span struct {
	MagicNumber    uint32
	Length         uint64
	SequenceNumber uint32
	RecordID       string
	DataStreams    []DataStream
	Checksum       uint32
}

type IndexEntry struct {
	Offset         uint64
	Span           *Span
	SequenceNumber uint64
}

type SpanFile struct {
	file     *os.File
	mmapData mmap.MMap
	// map from string id to offset of the record
	index          map[string]uint64
	freeMap        freeMap // Change from freeList to freeMap
	sequenceNumber uint32
	fileMutex      sync.Mutex
}

type FreeSpan struct {
	Offset uint64
	Length uint64
}

type OpenOptions struct {
	CreateIfNotExists bool
	OverwriteExisting bool
}

func OpenFile(filename string, options OpenOptions) (*SpanFile, error) {
	flags := os.O_RDWR
	if options.CreateIfNotExists {
		flags |= os.O_CREATE
	}
	if options.OverwriteExisting {
		flags |= os.O_TRUNC
	}

	file, err := os.OpenFile(filename, flags, 0666)
	if err != nil {
		log.Printf("Error opening file: %v", err)
		return nil, err
	}

	// Check the file size
	fileInfo, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}

	// If the file is zero bytes, initialize it with a minimal valid span header
	if fileInfo.Size() == 0 {
		// Create a minimal valid span
		span := &Span{
			MagicNumber: activeMagic,
		}

		// Serialize the span
		spanBytes, err := serializeSpan(span)
		if err != nil {
			file.Close()
			return nil, err
		}

		checkSum := calculateChecksum(spanBytes)
		spanBytes = append(spanBytes, byte(checkSum>>24), byte(checkSum>>16), byte(checkSum>>8), byte(checkSum))
		log.Printf("Write initial span:%v-%v/%v", 0, len(spanBytes), len(spanBytes))

		// Write the span to the file
		_, err = file.Write(spanBytes)
		if err != nil {
			file.Close()
			return nil, err
		}
	}

	// Memory map the file
	mmapData, err := mmap.MapRegion(file, -1, mmap.RDWR, 0, 0)
	if err != nil {
		log.Printf("Error mapping file: %v", err)
		file.Close()
		return nil, err
	}

	db := &SpanFile{
		file:           file,
		mmapData:       mmapData,
		index:          make(map[string]uint64),
		freeMap:        freeMap{freeSpaces: []space{}}, // Initialize freeMap
		sequenceNumber: 0,
	}

	err = db.scanFile()
	if err != nil {
		mmapData.Unmap()
		file.Close()
		return nil, err
	}

	return db, nil
}

func (db *SpanFile) scanFile() error {
	offset := 0
	fileSize := len(db.mmapData)
	highestSeqNum := uint32(0)
	sequences := make(map[string]uint32)
	for offset < fileSize {
		// Ensure there is enough data to read the magic number and length
		if offset+minSpanLength > fileSize {
			break // Not enough data for a complete span header
		}

		magicNumber := binary.BigEndian.Uint32(db.mmapData[offset : offset+4])

		// if the magic number of 0 then assume we are at the end of the file
		// and mark the rest as free.
		if magicNumber == 0 {
			log.Printf("Marking rest of file as free space: span%v:%v/%v", offset, fileSize-offset, fileSize)
			db.addFreeSpan(uint64(offset), uint64(fileSize-offset))
		}
		length, err := readUint32(db.mmapData, int(offset+4))
		//log.Printf("Scanning span at offset %d...%d\n", offset, length)

		// Ensure there is enough data for the entire span
		if err != nil || offset+int(length) > fileSize {
			break // Not enough data for the complete span
		}

		//log.Printf("Magicnumber %x", magicNumber)

		if magicNumber == activeMagic {
			spanData := db.mmapData[offset : offset+int(length)]
			if !verifyChecksum(spanData) {
				log.Printf("Checksum failed for span at offset %d\n", offset)
				offset += int(length)
				if length == 0 {
					return fmt.Errorf("length is 0; can't continue")
				}
				continue
			}

			span, err := parseSpan(spanData)
			if err != nil {
				log.Printf("Error parsing span: %v", err)
				offset += int(length)
				continue
			}

			log.Printf("USED: span:%v-%v/%v (%s)", offset, offset+int(length), length, span.RecordID)

			if span.SequenceNumber > highestSeqNum {
				highestSeqNum = span.SequenceNumber
			}

			existingSequence, exists := sequences[span.RecordID]
			if !exists || span.SequenceNumber > existingSequence {
				sequences[span.RecordID] = span.SequenceNumber
				db.index[span.RecordID] = uint64(offset)
			}
		} else if magicNumber == freeMagic {
			log.Printf("FREE: span:%v-%v/%v", offset, offset+int(length), length)
			db.addFreeSpan(uint64(offset), uint64(length))
		}

		offset += int(length)
		if length == 0 {
			return fmt.Errorf("length is 0; can't continue")
		}
	}

	db.sequenceNumber = highestSeqNum + 1
	return nil
}

// TODO: use freemap instead
func (db *SpanFile) addFreeSpan(offset, length uint64) {
	db.freeMap.markFree(int(offset), int(length)) // Use markFree from freeMap
}

func (db *SpanFile) RemoveRecord(recordID string) error {
	db.fileMutex.Lock()
	defer db.fileMutex.Unlock()

	// Find the offset of the record
	offset, exists := db.index[recordID]
	if !exists {
		return fmt.Errorf("record not found")
	}

	// Get the length of the span
	length, err := db.getSpanLength(int(offset))
	if err != nil {
		return err
	}

	log.Printf("Remove %s", recordID)
	log.Printf(" -->Mark span:%d-%d/%d as freed", offset, offset+length, length)

	// Mark the span as free
	err = db.markSpanAsFreed(offset)
	if err != nil {
		return err
	}

	// Add the span to the free list
	db.addFreeSpan(offset, length)

	// Remove the record from the index
	delete(db.index, recordID)

	return nil
}

func (db *SpanFile) WriteRecord(recordID string, dataStreams []DataStream) error {
	//TODO: Remove locks; we are protected at a higher level.
	db.fileMutex.Lock()
	defer db.fileMutex.Unlock()

	sequenceNumber := db.sequenceNumber
	db.sequenceNumber++

	span := &Span{
		MagicNumber:    activeMagic,
		SequenceNumber: sequenceNumber,
		RecordID:       recordID,
		DataStreams:    dataStreams,
	}

	spanBytes, err := serializeSpan(span)
	if err != nil {
		return err
	}

	offset, remaining, err := db.allocateSpan(len(spanBytes) + 4) //+4 for checksum
	if err != nil {
		return err
	}

	// If remaining is > 0 and < minSpanLength then we need to add padding
	// before the checksum, and add the length of this padding to the uint32
	// stored at offset 4 of the spanBytes.
	if remaining > 0 && remaining < minSpanLength {
		db.freeMap.markUsed(int(offset)+len(spanBytes)+4, int(remaining))
		padding := make([]byte, remaining)
		spanBytes = append(spanBytes, padding...)

		// Update the length in the spanBytes
		length := uint32(len(spanBytes) + 4) // +4 for the checksum
		binary.BigEndian.PutUint32(spanBytes[4:8], length)
	}

	checksum := calculateChecksum(spanBytes)
	spanBytes = append(spanBytes, byte(checksum>>24), byte(checksum>>16), byte(checksum>>8), byte(checksum))

	log.Printf("Write %s to span:%v-%v/%v", recordID, offset, offset+uint64(len(spanBytes)), len(spanBytes))
	if remaining > 0 && remaining < minSpanLength {
		log.Printf("--->Adding %v bytes of padding", remaining)
	}
	// if the remaining space is > minSpanLength then we need to write a free span
	// after it. This is simply the free magic number followed by the
	// length of the remaining space.
	if remaining >= minSpanLength {
		log.Printf(" -->Adding free space marker at span:%v-%v/%v", int(offset)+len(spanBytes), int(offset)+len(spanBytes)+int(remaining), remaining)
		freeSpan := make([]byte, 8)
		binary.BigEndian.PutUint32(freeSpan[0:4], freeMagic)
		binary.BigEndian.PutUint32(freeSpan[4:8], uint32(remaining))
		spanBytes = append(spanBytes, freeSpan...)
	}

	err = db.writeAt(spanBytes, offset)
	if err != nil {
		return err
	}

	if oldOffset, exists := db.index[recordID]; exists {
		oldLength, err := db.getSpanLength(int(oldOffset))
		if err != nil {
			return err
		}
		log.Printf(" -->Replaced record %s at span:%v-%v/%v)", recordID, oldOffset, oldOffset+oldLength, oldLength)
		err = db.markSpanAsFreed(oldOffset)
		if err != nil {
			return err
		}
		db.addFreeSpan(oldOffset, oldLength)
	}

	db.index[recordID] = offset

	return nil
}

func (db *SpanFile) allocateSpan(size int) (uint64, int64, error) {
	start, remaining, err := db.freeMap.getFreeRange(size)
	if err == nil {
		return uint64(start), remaining, nil
	}

	// Calculate the amount to expand the file by
	currentLength := len(db.mmapData)
	expandBy := max(4096, size, int(float64(currentLength)*0.05))

	// Append the required amount of space to the file
	err = db.appendToFile(make([]byte, expandBy))
	if err != nil {
		return 0, 0, err
	}

	// Return the new offset and remaining space
	return uint64(currentLength), int64(expandBy - size), nil
}

func (db *SpanFile) writeAt(data []byte, offset uint64) error {
	if offset+uint64(len(data)) > uint64(len(db.mmapData)) {
		log.Panic("writeAt: offset out of bounds")
	}

	copy(db.mmapData[offset:], data)
	return msync(db.mmapData[offset : offset+uint64(len(data))])
}

func (db *SpanFile) markSpanAsFreed(offset uint64) error {
	binary.BigEndian.PutUint32(db.mmapData[offset:offset+4], freeMagic)
	return msync(db.mmapData[offset : offset+4])
}

func (db *SpanFile) ReadRecord(recordID string) (*Span, error) {
	offset, exists := db.index[recordID]
	if !exists {
		return nil, fmt.Errorf("record not found")
	}
	return parseSpanAtOffset(db.mmapData, offset)
}

func (db *SpanFile) IterateRecords(callback func(recordID string, dataStreams []DataStream) error) error {
	for recordID, offset := range db.index {
		if recordID == "" {
			continue
		}
		span, err := parseSpanAtOffset(db.mmapData, offset)
		if err != nil {
			return err
		}

		err = callback(recordID, span.DataStreams)
		if err != nil {
			return err
		}
	}
	return nil
}

func (db *SpanFile) GetStats() (size uint64, numRecords int) {
	size = uint64(len(db.mmapData))
	numRecords = len(db.index) - 1 // Subtract 1 for the empty record
	return
}

func (db *SpanFile) DumpFile(output io.Writer) error {
	offset := uint64(0)
	fileSize := uint64(len(db.mmapData))
	for offset < fileSize {
		// Ensure there is enough data to read the magic number and length
		if offset+minSpanLength > fileSize {
			break // Not enough data for a complete span header
		}

		magicNumber := binary.BigEndian.Uint32(db.mmapData[offset : offset+4])
		length, err := readUint32(db.mmapData, int(offset+4))
		if err != nil {
			fmt.Fprintf(output, "Error reading length at offset %d: %v\n", offset, err)
			break
		}

		if magicNumber == freeMagic {
			fmt.Fprintf(output, "Offset: %d\n", offset)
			fmt.Fprintf(output, "Magic Number: %s\n", magicNumberToString(magicNumber))
			fmt.Fprintf(output, "Length: %d bytes\n", length)
			fmt.Fprintln(output)
			offset += uint64(length)
			continue
		}

		span, err := parseSpanAtOffset(db.mmapData, offset)
		if err != nil {
			fmt.Fprintf(output, "Magic number was %s\n", magicNumberToString(magicNumber))
			fmt.Fprintf(output, "Error parsing span at offset %d: %v\n", offset, err)
			break
		}

		fmt.Fprintf(output, "Offset: %d\n", offset)
		fmt.Fprintf(output, "Magic Number: %s\n", magicNumberToString(span.MagicNumber))
		fmt.Fprintf(output, "Length: %d bytes\n", span.Length)
		fmt.Fprintf(output, "Sequence Number: %d\n", span.SequenceNumber)
		fmt.Fprintf(output, "Record ID: %s\n", span.RecordID)
		fmt.Fprintf(output, "Data Streams:\n")
		for _, ds := range span.DataStreams {
			fmt.Fprintf(output, "  Stream ID: %d, Length: %d bytes\n", ds.StreamID, len(ds.Data))
		}
		fmt.Fprintf(output, "Checksum: %x\n", span.Checksum)
		fmt.Fprintln(output)

		offset += span.Length
	}
	return nil
}

func write7Code(buf []byte, n uint64) []byte {
	if n < 0x7f {
		buf = append(buf, byte(n))
	} else if n < 0x3fff {
		buf = append(buf, byte((n>>7)&0x7f|0x80))
		buf = append(buf, byte(n&0x7f))
	} else if n < 0x1fffff {
		buf = append(buf, byte((n>>14)&0x7f|0x80))
		buf = append(buf, byte((n>>7)&0x7f|0x80))
		buf = append(buf, byte(n&0x7f))
	} else if n < 0xfffffff {
		buf = append(buf, byte((n>>21)&0x7f|0x80))
		buf = append(buf, byte((n>>14)&0x7f|0x80))
		buf = append(buf, byte((n>>7)&0x7f|0x80))
		buf = append(buf, byte(n&0x7f))
	} else if n < 0x7ffffffff {
		buf = append(buf, byte((n>>28)&0x7f|0x80))
		buf = append(buf, byte((n>>21)&0x7f|0x80))
		buf = append(buf, byte((n>>14)&0x7f|0x80))
		buf = append(buf, byte((n>>7)&0x7f|0x80))
		buf = append(buf, byte(n&0x7f))
	} else if n < 0x3ffffffffff {
		buf = append(buf, byte((n>>35)&0x7f|0x80))
		buf = append(buf, byte((n>>28)&0x7f|0x80))
		buf = append(buf, byte((n>>21)&0x7f|0x80))
		buf = append(buf, byte((n>>14)&0x7f|0x80))
		buf = append(buf, byte((n>>7)&0x7f|0x80))
		buf = append(buf, byte(n&0x7f))
	} else if n < 0x1ffffffffffff {
		buf = append(buf, byte((n>>42)&0x7f|0x80))
		buf = append(buf, byte((n>>35)&0x7f|0x80))
		buf = append(buf, byte((n>>28)&0x7f|0x80))
		buf = append(buf, byte((n>>21)&0x7f|0x80))
		buf = append(buf, byte((n>>14)&0x7f|0x80))
		buf = append(buf, byte((n>>7)&0x7f|0x80))
		buf = append(buf, byte(n&0x7f))
	} else if n < 0xffffffffffffff {
		buf = append(buf, byte((n>>49)&0x7f|0x80))
		buf = append(buf, byte((n>>42)&0x7f|0x80))
		buf = append(buf, byte((n>>35)&0x7f|0x80))
		buf = append(buf, byte((n>>28)&0x7f|0x80))
		buf = append(buf, byte((n>>21)&0x7f|0x80))
		buf = append(buf, byte((n>>14)&0x7f|0x80))
		buf = append(buf, byte((n>>7)&0x7f|0x80))
		buf = append(buf, byte(n&0x7f))
	} else {
		buf = append(buf, byte((n>>56)&0x7f|0x80))
		buf = append(buf, byte((n>>49)&0x7f|0x80))
		buf = append(buf, byte((n>>42)&0x7f|0x80))
		buf = append(buf, byte((n>>35)&0x7f|0x80))
		buf = append(buf, byte((n>>28)&0x7f|0x80))
		buf = append(buf, byte((n>>21)&0x7f|0x80))
		buf = append(buf, byte((n>>14)&0x7f|0x80))
		buf = append(buf, byte((n>>7)&0x7f|0x80))
		buf = append(buf, byte(n&0x7f))
	}
	return buf
}

func read7Code(buff []byte, offset int) (result uint64, newOffset int, err error) {
	for ; offset < len(buff); offset++ {
		d := uint64(buff[offset])
		result = (result << 7) | d&0x7f
		if d&0x80 == 0 {
			return result, offset + 1, nil
		}
	}
	return 0, 0, fmt.Errorf("buffer too short to read unsigned value")
}

func lengthOf7Code(n uint64) uint64 {
	switch {
	case n < 0x7f:
		return 1
	case n < 0x3fff:
		return 2
	case n < 0x1fffff:
		return 3
	case n < 0xfffffff:
		return 4
	case n < 0x7ffffffff:
		return 5
	case n < 0x3ffffffffff:
		return 6
	case n < 0x1ffffffffffff:
		return 7
	case n < 0xffffffffffffff:
		return 8
	case n < 0x7fffffffffffffff:
		return 9
	default:
		return 10
	}
}

func writeUint32(buf []byte, n uint32) []byte {
	buf = append(buf, byte(n>>24))
	buf = append(buf, byte(n>>16))
	buf = append(buf, byte(n>>8))
	buf = append(buf, byte(n))
	return buf
}

func readUint32(buf []byte, offset int) (uint32, error) {
	if offset+4 > len(buf) {
		return 0, fmt.Errorf("record too short to contain length")
	}
	return uint32(buf[offset])<<24 | uint32(buf[offset+1])<<16 |
		uint32(buf[offset+2])<<8 | uint32(buf[offset+3]), nil
}

func serializeSpan(span *Span) ([]byte, error) {

	recordIDBytes := []byte(span.RecordID)

	// Calculate Length
	length := 4 + 4 + // magic + length
		lengthOf7Code(uint64(span.SequenceNumber)) +
		lengthOf7Code(uint64(len(recordIDBytes))) +
		uint64(len(recordIDBytes)) +
		1 + // DataStreamCount
		4 // Checksum

	for _, stream := range span.DataStreams {
		length += 1 + lengthOf7Code(uint64(len(stream.Data))) + uint64(len(stream.Data))
	}

	//log.Printf("Encoded length is %v l1=%v", length, l1)

	buf := make([]byte, 0, length)

	// Serialize MagicNumber
	buf = writeUint32(buf, span.MagicNumber)

	// length
	buf = writeUint32(buf, uint32(length))

	// sequence number
	buf = write7Code(buf, uint64(span.SequenceNumber))

	// Serialize RecordID Length and RecordID
	buf = write7Code(buf, uint64(len(recordIDBytes)))
	buf = append(buf, recordIDBytes...)

	// Serialize Number of Data Streams
	buf = append(buf, byte(len(span.DataStreams)))

	// Serialize Data Streams
	for _, ds := range span.DataStreams {
		buf = append(buf, ds.StreamID)
		buf = write7Code(buf, uint64(len(ds.Data)))
		buf = append(buf, ds.Data...)
	}

	//log.Printf("length without checksum is %d", len(buf))

	// Debugging output
	//fmt.Printf("Serialized span length: %d bytes\n", length+4) // plus unknown padding?

	return buf, nil
}

func parseSpan(data []byte) (*Span, error) {
	if len(data) < minSpanLength {
		return nil, fmt.Errorf("data too short to be a valid span")
	}

	span := &Span{}
	span.MagicNumber = binary.BigEndian.Uint32(data[:4])
	at := 4

	if span.MagicNumber != activeMagic {
		return nil, fmt.Errorf("invalid magic number")
	}

	var err error
	var l uint32
	l, err = readUint32(data, at)
	at += 4
	span.Length = uint64(l)
	if err != nil {
		return nil, err
	}

	// Ensure the data slice is long enough for the entire span
	if int(span.Length) > len(data) {
		return nil, fmt.Errorf("data too short for span length, data=%v lengthRead=%v", len(data), span.Length)
	}

	if !verifyChecksum(data[:span.Length]) {
		return nil, fmt.Errorf("checksum failed")
	}

	// Parse Sequence number
	seq, at, err := read7Code(data, at)
	if err != nil {
		return nil, err
	}
	span.SequenceNumber = uint32(seq)

	// Parse RecordID
	idlength, at, err := read7Code(data, at)
	if err != nil {
		return nil, err
	}
	span.RecordID = string(data[at : at+int(idlength)])
	at += int(idlength)

	// Parse Number of Data Streams
	numStreams := int(data[at])
	at++

	//log.Printf("IDlength is %d, RecordID is %s, numStreams is %d\n", idlength, span.RecordID, numStreams)
	// Parse Data Streams
	for i := 0; i < numStreams; i++ {
		if at >= len(data) {
			return nil, fmt.Errorf("data too short to contain all streams")
		}
		streamID := data[at]
		at++

		streamLen, at, err := read7Code(data, at)
		if err != nil {
			return nil, err
		}

		if at+int(streamLen) > len(data) {
			return nil, fmt.Errorf("data too short for stream data")
		}

		streamData := data[at : at+int(streamLen)]
		at += int(streamLen)

		span.DataStreams = append(span.DataStreams, DataStream{
			StreamID: streamID,
			Data:     streamData,
		})
	}

	// Parse Checksum
	if at+4 > len(data) {
		return nil, fmt.Errorf("data too short for checksum")
	}
	at = int(span.Length) - 4
	span.Checksum = binary.BigEndian.Uint32(data[at : at+4])

	return span, nil
}

func parseSpanAtOffset(data []byte, offset uint64) (*Span, error) {
	if offset >= uint64(len(data)) {
		return nil, fmt.Errorf("offset out of bounds")
	}
	return parseSpan(data[offset:])
}

func (db *SpanFile) getSpanLength(offset int) (uint64, error) {
	// Read the length of the span
	length, err := readUint32(db.mmapData, offset+4)
	if err != nil {
		return 0, err
	}
	return uint64(length), nil
}

func calculateChecksum(data []byte) uint32 {
	return crc32.ChecksumIEEE(data)

}

func verifyChecksum(data []byte) bool {
	l := len(data)
	if l < 4 {
		return false
	}
	expectedChecksum := uint32(data[l-4])<<24 | uint32(data[l-3])<<16 | uint32(data[l-2])<<8 | uint32(data[l-1])
	actualChecksum := calculateChecksum(data[:l-4])
	return actualChecksum == expectedChecksum
}

func (db *SpanFile) appendToFile(data []byte) error {
	// Ensure the file is large enough
	_, err := db.file.WriteAt(data, int64(len(db.mmapData)))
	if err != nil {
		return err
	}

	// Remap the file
	db.mmapData.Unmap()
	db.mmapData, err = mmap.Map(db.file, mmap.RDWR, 0)
	if err != nil {
		return err
	}

	return nil
}

func msync(_ []byte) error {
	// Implement msync logic
	// This is a placeholder implementation
	return nil
}

func magicNumberToString(magic uint32) string {
	switch magic {
	case activeMagic:
		return "SPAN"
	case freeMagic:
		return "FREE"
	default:
		return "UNKNOWN"
	}
}