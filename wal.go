package wal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	lru "github.com/hashicorp/golang-lru/v2"
)

const (
	initialSegmentFileID = 1
)

var (
	ErrDataSizeTooLarge    = errors.New("the data size must smaller than segment file limit")
	ErrPendingSizeTooLarge = errors.New("the upper bound of pending writes can't larger than segment size")
)

type WAL struct {
	activeSegment     *segment                 // active segment file, used for new incoming writes.
	olderSegments     map[SegSerialID]*segment // older segment files, only used for read.
	options           Options
	mu                sync.RWMutex
	blockCache        *lru.Cache[uint64, []byte]
	bytesWrite        uint32
	renameIds         []SegSerialID
	pendingWrites     [][]byte
	pendingSize       int64
	pendingWritesLock sync.Mutex
}

type Reader struct {
	segmentReaders []*segmentReader
	currentReader  int
}

func Open(options Options) (*WAL, error) {
	if !strings.HasPrefix(options.DiskFileExtension, ".") {
		return nil, fmt.Errorf("invalid file extension")
	}
	if options.BlockCache > uint32(options.SegmentSize) {
		return nil, fmt.Errorf("BlockCache must be smaller than SegmentSize")
	}
	wal := &WAL{
		options:       options,
		olderSegments: make(map[SegSerialID]*segment),
		pendingWrites: make([][]byte, 0),
	}

	// create the directory if not exists.
	if err := os.MkdirAll(options.DirPath, os.ModePerm); err != nil {
		return nil, err
	}
	if options.BlockCache > 0 {
		var lruSize = options.BlockCache / blockSize
		if options.BlockCache%blockSize != 0 {
			lruSize += 1
		}
		cache, err := lru.New[uint64, []byte](int(lruSize))
		if err != nil {
			return nil, err
		}
		wal.blockCache = cache
	}
	// iterate the dir and open all segment files.
	entries, err := os.ReadDir(options.DirPath)
	if err != nil {
		return nil, err
	}

	// get all segment file ids.
	var segmentIDs []int
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		var id int
		_, err := fmt.Sscanf(entry.Name(), "%d"+options.DiskFileExtension, &id)
		if err != nil {
			continue
		}
		segmentIDs = append(segmentIDs, id)
	}

	// empty directory, just initialize a new segment file.
	if len(segmentIDs) == 0 {
		segment, err := openSegmentFile(options.DirPath, options.DiskFileExtension,
			initialSegmentFileID, wal.blockCache)
		if err != nil {
			return nil, err
		}
		wal.activeSegment = segment
	} else {
		// open the segment files in order, get the max one as the active segment file.
		sort.Ints(segmentIDs)

		for i, segId := range segmentIDs {
			segment, err := openSegmentFile(options.DirPath, options.DiskFileExtension,
				uint32(segId), wal.blockCache)
			if err != nil {
				return nil, err
			}
			if i == len(segmentIDs)-1 {
				wal.activeSegment = segment
			} else {
				wal.olderSegments[segment.id] = segment
			}
		}
	}

	return wal, nil
}

func SegmentFileName(dirPath string, extName string, id SegSerialID) string {
	return filepath.Join(dirPath, fmt.Sprintf("%09d"+extName, id))
}

func (wal *WAL) OpenNewActiveSegment() error {
	wal.mu.Lock()
	defer wal.mu.Unlock()
	// sync the active segment file.
	if err := wal.activeSegment.Sync(); err != nil {
		return err
	}
	// create a new segment file and set it as the active one.
	segment, err := openSegmentFile(wal.options.DirPath, wal.options.DiskFileExtension,
		wal.activeSegment.id+1, wal.blockCache)
	if err != nil {
		return err
	}
	wal.olderSegments[wal.activeSegment.id] = wal.activeSegment
	wal.activeSegment = segment
	return nil
}

func (wal *WAL) ActiveSegmentID() SegSerialID {
	wal.mu.RLock()
	defer wal.mu.RUnlock()

	return wal.activeSegment.id
}

// IsEmpty returns whether the WAL is empty.
// Only there is only one empty active segment file, which means the WAL is empty.
func (wal *WAL) IsEmpty() bool {
	wal.mu.RLock()
	defer wal.mu.RUnlock()

	return len(wal.olderSegments) == 0 && wal.activeSegment.Size() == 0
}

// NewReaderWithMax returns a new reader for the WAL,
func (wal *WAL) NewReaderWithMax(segId SegSerialID) *Reader {
	wal.mu.RLock()
	defer wal.mu.RUnlock()

	// get all segment readers.
	var segmentReaders []*segmentReader
	for _, segment := range wal.olderSegments {
		if segId == 0 || segment.id <= segId {
			reader := segment.NewReader()
			segmentReaders = append(segmentReaders, reader)
		}
	}
	if segId == 0 || wal.activeSegment.id <= segId {
		reader := wal.activeSegment.NewReader()
		segmentReaders = append(segmentReaders, reader)
	}

	// sort the segment readers by segment id.
	sort.Slice(segmentReaders, func(i, j int) bool {
		return segmentReaders[i].segment.id < segmentReaders[j].segment.id
	})

	return &Reader{
		segmentReaders: segmentReaders,
		currentReader:  0,
	}
}

// NewReaderWithStart returns a new reader for the WAL,
// and the reader will only read the data from the segment file
// whose position is greater than or equal to the given position.
func (wal *WAL) NewReaderWithStart(startPos *ChunkPosition) (*Reader, error) {
	if startPos == nil {
		return nil, errors.New("start position is nil")
	}
	wal.mu.RLock()
	defer wal.mu.RUnlock()

	reader := wal.NewReader()
	for {
		// skip the segment readers whose id is less than the given position's segment id.
		if reader.CurrentSegmentId() < startPos.SegmentId {
			reader.SkipCurrentSegment()
			continue
		}
		// skip the chunk whose position is less than the given position.
		currentPos := reader.CurrentChunkPosition()
		if currentPos.BlockNumber >= startPos.BlockNumber &&
			currentPos.ChunkOffset >= startPos.ChunkOffset {
			break
		}
		// call Next to find again.
		if _, _, err := reader.Next(); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
	}
	return reader, nil
}

// NewReader returns a new reader for the WAL.
// It will iterate all segment files and read all data from them.
func (wal *WAL) NewReader() *Reader {
	return wal.NewReaderWithMax(0)
}

// Next returns the next chunk data and its position in the WAL.
// If there is no data, io.EOF will be returned.
//
// The position can be used to read the data from the segment file.
func (r *Reader) Next() ([]byte, *ChunkPosition, error) {
	if r.currentReader >= len(r.segmentReaders) {
		return nil, nil, io.EOF
	}

	data, position, err := r.segmentReaders[r.currentReader].Next()
	if err == io.EOF {
		r.currentReader++
		return r.Next()
	}
	return data, position, err
}

func (r *Reader) SkipCurrentSegment() {
	r.currentReader++
}

func (r *Reader) CurrentSegmentId() SegSerialID {
	return r.segmentReaders[r.currentReader].segment.id
}

func (r *Reader) CurrentChunkPosition() *ChunkPosition {
	reader := r.segmentReaders[r.currentReader]
	return &ChunkPosition{
		SegmentId:   reader.segment.id,
		BlockNumber: reader.blockNumber,
		ChunkOffset: reader.chunkOffset,
	}
}

func (wal *WAL) ClearPendingWrites() {
	wal.pendingWritesLock.Lock()
	defer wal.pendingWritesLock.Unlock()

	wal.pendingSize = 0
	wal.pendingWrites = wal.pendingWrites[:0]
}

func (wal *WAL) PendingWrites(data []byte) {
	wal.pendingWritesLock.Lock()
	defer wal.pendingWritesLock.Unlock()

	size := wal.maxDataWriteSize(int64(len(data)))
	wal.pendingSize += size
	wal.pendingWrites = append(wal.pendingWrites, data)
}

func (wal *WAL) rotateActiveSegment() error {
	if err := wal.activeSegment.Sync(); err != nil {
		return err
	}
	wal.bytesWrite = 0
	segment, err := openSegmentFile(wal.options.DirPath, wal.options.DiskFileExtension,
		wal.activeSegment.id+1, wal.blockCache)
	if err != nil {
		return err
	}
	wal.olderSegments[wal.activeSegment.id] = wal.activeSegment
	wal.activeSegment = segment
	return nil
}

func (wal *WAL) WriteAll() ([]*ChunkPosition, error) {
	if len(wal.pendingWrites) == 0 {
		return make([]*ChunkPosition, 0), nil
	}

	wal.mu.Lock()
	defer func() {
		wal.ClearPendingWrites()
		wal.mu.Unlock()
	}()

	// if the pending size is still larger than segment size, return error
	if wal.pendingSize > wal.options.SegmentSize {
		return nil, ErrPendingSizeTooLarge
	}

	// if the active segment file is full, sync it and create a new one.
	if wal.activeSegment.Size()+wal.pendingSize > wal.options.SegmentSize {
		if err := wal.rotateActiveSegment(); err != nil {
			return nil, err
		}
	}

	// write all data to the active segment file.
	positions, err := wal.activeSegment.writeAll(wal.pendingWrites)
	if err != nil {
		return nil, err
	}

	return positions, nil
}

// Write writes the data to the WAL.
// Actually, it writes the data to the active segment file.
// It returns the position of the data in the WAL, and an error if any.
func (wal *WAL) Write(data []byte) (*ChunkPosition, error) {
	wal.mu.Lock()
	defer wal.mu.Unlock()
	if int64(len(data))+chunkHeaderSize > wal.options.SegmentSize {
		return nil, ErrDataSizeTooLarge
	}
	// if the active segment file is full, sync it and create a new one.
	if wal.isFull(int64(len(data))) {
		if err := wal.rotateActiveSegment(); err != nil {
			return nil, err
		}
	}

	// write the data to the active segment file.
	position, err := wal.activeSegment.Write(data)
	if err != nil {
		return nil, err
	}

	// update the bytesWrite field.
	wal.bytesWrite += position.ChunkSize

	// sync the active segment file if needed.
	var needSync = wal.options.DiskFlushSync
	if !needSync && wal.options.BytesPerSync > 0 {
		needSync = wal.bytesWrite >= wal.options.BytesPerSync
	}
	if needSync {
		if err := wal.activeSegment.Sync(); err != nil {
			return nil, err
		}
		wal.bytesWrite = 0
	}

	return position, nil
}

// Read reads the data from the WAL according to the given position.
func (wal *WAL) Read(pos *ChunkPosition) ([]byte, error) {
	wal.mu.RLock()
	defer wal.mu.RUnlock()

	// find the segment file according to the position.
	var segment *segment
	if pos.SegmentId == wal.activeSegment.id {
		segment = wal.activeSegment
	} else {
		segment = wal.olderSegments[pos.SegmentId]
	}

	if segment == nil {
		return nil, fmt.Errorf("segment file %d%s not found", pos.SegmentId, wal.options.DiskFileExtension)
	}

	// read the data from the segment file.
	return segment.Read(pos.BlockNumber, pos.ChunkOffset)
}

// Close closes the WAL.
func (wal *WAL) Close() error {
	wal.mu.Lock()
	defer wal.mu.Unlock()

	// close all segment files.
	for _, segment := range wal.olderSegments {
		if err := segment.Close(); err != nil {
			return err
		}
		wal.renameIds = append(wal.renameIds, segment.id)
	}
	wal.olderSegments = nil

	wal.renameIds = append(wal.renameIds, wal.activeSegment.id)
	// close the active segment file.
	return wal.activeSegment.Close()
}

// Delete deletes all segment files of the WAL.
func (wal *WAL) Delete() error {
	wal.mu.Lock()
	defer wal.mu.Unlock()

	// delete all segment files.
	for _, segment := range wal.olderSegments {
		if err := segment.Remove(); err != nil {
			return err
		}
	}
	wal.olderSegments = nil

	// delete the active segment file.
	return wal.activeSegment.Remove()
}

// Sync syncs the active segment file to stable storage like disk.
func (wal *WAL) Sync() error {
	wal.mu.Lock()
	defer wal.mu.Unlock()

	return wal.activeSegment.Sync()
}

func (wal *WAL) RenameFileExt(ext string) error {
	if !strings.HasPrefix(ext, ".") {
		return fmt.Errorf("file extension must start with '.'")
	}
	wal.mu.Lock()
	defer wal.mu.Unlock()

	renameFile := func(id SegSerialID) error {
		oldName := SegmentFileName(wal.options.DirPath, wal.options.DiskFileExtension, id)
		newName := SegmentFileName(wal.options.DirPath, ext, id)
		return os.Rename(oldName, newName)
	}

	for _, id := range wal.renameIds {
		if err := renameFile(id); err != nil {
			return err
		}
	}

	wal.options.DiskFileExtension = ext
	return nil
}

func (wal *WAL) isFull(delta int64) bool {
	return wal.activeSegment.Size()+wal.maxDataWriteSize(delta) > wal.options.SegmentSize
}

func (wal *WAL) maxDataWriteSize(size int64) int64 {
	return chunkHeaderSize + size + (size/blockSize+1)*chunkHeaderSize
}
