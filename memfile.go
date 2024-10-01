package main

import (
	"log"
	"os"

	"github.com/go-mmap/mmap"
)

// The memory file consists of a header followed by a series of records.
// Each record is:
// uint64 - total length of record
// uint64 - ID, or deleted is all 0xffffffffffffffff

type memfile struct {
	*mmap.File
	//Header size which we ignore
	headerSize int

	// offsets of each record id into the file
	idOffsets map[uint64]int64

	freemap FreeMap

	name string
}

func createMemFile(name string, headerSize int) (*memfile, error) {
	f, err := mmap.OpenFile(name, mmap.Read|mmap.Write)
	if err != nil {
		return nil, err
	}
	ret := &memfile{File: f,
		headerSize: headerSize,
		name:       name,
	}

	return ret, nil
}

// check if the file is at least the given length, and if not, extend it
// and remap the file
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

func (mf *memfile) addRecord(id uint64, data []byte) {
	// Calculate the total length of the record
	recordLength := 16 + len(data) // 8 bytes for length, 8 bytes for ID

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
	binary.LittleEndian.PutUint64(mf.Data[offset:], uint64(recordLength))
	binary.LittleEndian.PutUint64(mf.Data[offset+8:], id)
	copy(mf.Data[offset+16:], data)

	// Sync the file to disk
	if err := mf.File.Sync(); err != nil {
		log.Panic(err)
	}

	// If the record already existed, mark the old space as free
	if oldOffset, exists := mf.idOffsets[id]; exists {
		binary.LittleEndian.PutUint64(mf.Data[oldOffset+8:], 0xffffffffffffffff)
		oldLength := binary.LittleEndian.Uint64(mf.Data[oldOffset:])
		mf.freemap.markFree(int(oldOffset), int(oldLength))
	}

	// Update the idOffsets map
	mf.idOffsets[id] = int64(offset)
}
