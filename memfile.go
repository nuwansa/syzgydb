package syzygy

import (
	"encoding/binary"
	"errors"
	"log"
	"os"

	"github.com/go-mmap/mmap"
)

// The memory file consists of a header followed by a series of records.
// Each record is:
// uint64 - total length of record
// uint64 - ID, or deleted is all 0xffffffffffffffff

const debug = true

type memfile struct {
	*mmap.File
	// Header size which we ignore
	headerSize int64

	// offsets of each record id into the file
	idOffsets map[uint64]int64

	freemap FreeMap

	name string
}

/*
deleteRecord marks a record as deleted and frees its space.

Parameters:
- id: The ID of the record to delete.

Returns:
- An error if the record is not found.
*/
func (mf *memfile) deleteRecord(id uint64) error {

	// Check if the record ID exists
	offset, exists := mf.idOffsets[id]
	if !exists {
		return errors.New("record not found")
	}

	// Mark the record as deleted
	mf.writeUint64(offset+8, 0xffffffffffffffff)

	// Mark the space as free
	recordLength := mf.readUint64(offset)
	mf.freemap.markFree(int(offset), int(recordLength))

	// Remove the record ID from the idOffsets map
	delete(mf.idOffsets, id)

	return nil
}

/*
createMemFile creates a new memory-mapped file and writes the header if the file is new.

Parameters:
- name: The name of the file.
- header: The header data to write if the file is new.

Returns:
- A pointer to the created memfile.
- An error if the file cannot be created.
*/
func createMemFile(name string, header []byte) (*memfile, error) {
	// Check if the file exists
	if _, err := os.Stat(name); os.IsNotExist(err) {
		// Create the file if it doesn't exist
		file, createErr := os.Create(name)
		if createErr != nil {
			return nil, createErr
		}
		file.Close()
	}

	// Open the file with mmap
	f, err := mmap.OpenFile(name, mmap.Read|mmap.Write)
	if err != nil {
		return nil, err
	}

	ret := &memfile{
		File:       f,
		idOffsets:  make(map[uint64]int64),
		headerSize: int64(len(header)),
		name:       name,
	}

	// Check if the file is new by checking its size
	if ret.Len() == 0 {
		ret.ensureLength(len(header))
		// Write the header to the file
		if _, err := ret.WriteAt(header, 0); err != nil {
			return nil, err
		}
		ret.Sync()
	}

	return ret, nil
}

/*
ensureLength checks if the file is at least the given length, and if not,
extends it and remaps the file.

Parameters:
- length: The minimum length the file should be.
*/
func (mf *memfile) ensureLength(length int) {
	curSize := mf.File.Len()
	if curSize >= length {
		return
	}

	length += 4096

	// Close the current memory-mapped file
	if err := mf.File.Close(); err != nil {
		log.Panic(err)
	}

	// Open the file on disk
	file, err := os.OpenFile(mf.name, os.O_RDWR, 0644)
	if err != nil {
		log.Panic(err)
	}
	defer file.Close()

	// Increase the file size
	if err := file.Truncate(int64(length)); err != nil {
		log.Panic(err)
	}

	// Update freemap with the extended range
	mf.freemap.markFree(int(curSize), length-int(curSize))

	// Re-obtain the memory-mapped file
	mf.File, err = mmap.OpenFile(mf.name, mmap.Read|mmap.Write)
	if err != nil {
		log.Panic(err)
	}
}

/*
addRecord adds a new record to the memory-mapped file.

Parameters:
- id: The ID of the record.
- data: The data to be stored in the record.
*/
func (mf *memfile) addRecord(id uint64, data []byte) bool {
	// Calculate the total length of the record
	recordLength := 16 + len(data) // 8 bytes for length, 8 bytes for ID

	// Determine if the record was newly added or updated
	wasNew := true

	// Find a free location for the new record
	start, err := mf.freemap.getFreeRange(recordLength)
	if err != nil {
		// If no free space, ensure the file is large enough
		mf.ensureLength(mf.File.Len() + recordLength)
		start, err = mf.freemap.getFreeRange(recordLength)
		if err != nil {
			log.Panic("Failed to allocate space for the new record")
		}
	}

	// Write the record to the file
	offset := start + mf.headerSize
	mf.writeUint64(offset, uint64(recordLength))

	// Write the ID
	mf.writeUint64(offset+8, id)

	// Write the data
	mf.WriteAt(data, offset+16)

	//mf.File.Sync()

	// If the record already existed, mark the old space as free
	if oldOffset, exists := mf.idOffsets[id]; exists {
		mf.writeUint64(oldOffset, 0xffffffffffffffff)
		oldLength := mf.readUint64((oldOffset))
		mf.freemap.markFree(int(oldOffset), int(oldLength))
		wasNew = false
	}

	// Update the idOffsets map
	mf.idOffsets[id] = int64(offset)

	return wasNew
}

/*
readUint64 reads an unsigned 64-bit integer from the specified offset.

Parameters:
- offset: The offset from which to read.

Returns:
- The unsigned 64-bit integer read from the file.
*/
func (mf *memfile) readUint64(offset int64) uint64 {
	// Read 8 bytes from the specified offset
	buf := make([]byte, 8)
	mf.ReadAt(buf, offset)
	return binary.BigEndian.Uint64(buf)
}

/*
readRecord reads a record by its ID.

Parameters:
- id: The ID of the record to read.

Returns:
- The data of the record.
- An error if the record is not found.
*/
func (mf *memfile) readRecord(id uint64) ([]byte, error) {
	// Check if the record ID exists
	offset, exists := mf.idOffsets[id]
	if !exists {
		return nil, errors.New("record not found")
	}

	// Read the total length of the record
	recordLength := mf.readUint64(offset)

	// Read the record data
	data := make([]byte, recordLength-16) // Subtract 16 bytes for length and ID
	mf.ReadAt(data, offset+16)

	return data, nil
}

/*
writeByte writes a single byte to the specified offset.

Parameters:
- offset: The offset at which to write.
- value: The byte value to write.
*/
func (mf *memfile) writeByte(offset int64, value byte) {
	mf.WriteAt([]byte{value}, offset)
}

/*
writeUint32 writes an unsigned 32-bit integer to the specified offset.

Parameters:
- offset: The offset at which to write.
- value: The unsigned 32-bit integer to write.
*/
func (mf *memfile) writeUint32(offset int64, value uint32) {
	// Convert value to a byte slice
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, value)
	mf.WriteAt(buf, offset)
}

/*
readUint32 reads an unsigned 32-bit integer from the specified offset.

Parameters:
- offset: The offset from which to read.

Returns:
- The unsigned 32-bit integer read from the file.
*/
func (mf *memfile) readUint32(offset int64) uint32 {
	// Read 4 bytes from the specified offset
	buf := make([]byte, 4)
	mf.ReadAt(buf, offset)
	return binary.BigEndian.Uint32(buf)
}

/*
writeUint64 writes an unsigned 64-bit integer to the specified offset.

Parameters:
- offset: The offset at which to write.
- value: The unsigned 64-bit integer to write.
*/
func (mf *memfile) writeUint64(offset int64, value uint64) {
	// use mf.File.WriteByte() to write the value to the file
	// assume that it is already large enough.

	// convert value to a byte slice
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, value)
	mf.WriteAt(buf, int64(offset))
}
