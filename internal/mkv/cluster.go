package mkv

import (
	"bytes"
	"compress/zlib"
	"errors"
	"io"
)

// Block is one SimpleBlock, or the Block of a BlockGroup, of a Cluster.
type Block struct {
	Track uint64
	// Timestamp is the Cluster's timestamp plus the block's, in the file's
	// timestamp units: the block's presentation time.
	Timestamp   int64
	Keyframe    bool
	Invisible   bool
	Discardable bool
	// Frames are the block's frames, laced frames apart, the track's content
	// encodings undone; nil for a block the reader was told to skip.
	Frames [][]byte
	// Duration is a BlockGroup's BlockDuration, -1 without one.
	Duration int64
	// Pos is the absolute position of the block's element (the SimpleBlock,
	// or its BlockGroup) and Cluster that of its Cluster.
	Pos     int64
	Cluster int64
}

// Reader reads the blocks of a file's Clusters in file order, from a
// Cluster's position on, by ReadAt in chunks: what a segment of the stream
// copy needs, and no more than it reads.
type Reader struct {
	f   *File
	r   io.ReaderAt
	pos int64 // the next element
	end int64 // where reading stops

	// Want says whose frames Next returns; the others' blocks come with their
	// headers only (nil: every track's).
	Want func(track uint64) bool
	// Chunk is how much one read reads at least.
	Chunk int
	// MaxInflated bounds the bytes the frames the Reader returns inflate to
	// (a track's zlib content encoding), all blocks together; 0:
	// DefaultMaxInflated. One block inflates to MaxBlockInflated at most.
	MaxInflated int64
	inflated    int64

	buf   []byte
	bufAt int64

	inCluster  bool
	clusterAt  int64
	clusterEnd int64 // -1: unknown size, it ends at a top-level element
	clusterTS  int64
	read       int64 // bytes read so far
}

// DefaultChunk is a Reader's read size unless set.
const DefaultChunk = 1 << 20

// MaxBlock bounds one block (a SimpleBlock's or a Block's payload): a block
// said to be larger is damage, not a frame, and nothing is read for it.
const MaxBlock = 256 << 20

// The bounds of what frames inflate to (zlib): a block's frames together,
// and a Reader's blocks together unless its MaxInflated says otherwise. No
// real block comes near them — zlib compresses subtitles, rarely anything
// else — a block made to inflate to gigabytes (laced frames of zeros) does.
const (
	MaxBlockInflated   = 64 << 20
	DefaultMaxInflated = 256 << 20
)

// Blocks is a Reader of r (the file f) from the Cluster at the absolute
// position from to the end of the Segment.
func (f *File) Blocks(r io.ReaderAt, from int64) *Reader {
	return &Reader{f: f, r: r, pos: from, end: f.SegmentEnd, Chunk: DefaultChunk, clusterEnd: -1}
}

// Pos is the position of the next element the reader reads.
func (rd *Reader) Pos() int64 { return rd.pos }

// BytesRead is how many bytes the reader has read from the file.
func (rd *Reader) BytesRead() int64 { return rd.read }

// ensure makes the buffer hold [at, at+n), reading at least Chunk bytes from
// at when it does not (less at the end of the file); ok false when the file
// ends before at+n.
func (rd *Reader) ensure(at int64, n int64) (bool, error) {
	if at < 0 || n < 0 || at > rd.f.Size || n > rd.f.Size-at {
		return false, nil // past the end of the file
	}
	if n > MaxBlock+blockHead {
		return false, formatErr("a read of %d bytes at %d", n, at)
	}
	if at >= rd.bufAt && at+n <= rd.bufAt+int64(len(rd.buf)) {
		return true, nil
	}
	size := max(n, int64(rd.Chunk))
	if at+size > rd.f.Size {
		size = rd.f.Size - at
	}
	b := make([]byte, size)
	got, err := rd.r.ReadAt(b, at)
	rd.read += int64(got)
	if err != nil && err != io.EOF {
		return false, err
	}
	if int64(got) < n {
		return false, nil
	}
	rd.buf, rd.bufAt = b[:got], at
	return true, nil
}

// bytesAt is the buffer from at on, holding at least n bytes.
func (rd *Reader) bytesAt(at, n int64) ([]byte, bool, error) {
	ok, err := rd.ensure(at, n)
	if !ok || err != nil {
		return nil, false, err
	}
	return rd.buf[at-rd.bufAt:], true, nil
}

// headerAt reads the element header at the absolute position at; ok false
// at the end of the file, or for a header it cuts short.
func (rd *Reader) headerAt(at int64) (header, bool, error) {
	if at < 0 || at >= rd.f.Size {
		return header{}, false, nil
	}
	// An ID and a size take 12 bytes at most: the buffer holds them, or all
	// the file has left.
	b, ok, err := rd.bytesAt(at, min(12, rd.f.Size-at))
	if !ok || err != nil {
		return header{}, false, err
	}
	return readHeader(b)
}

// Next returns the next block; io.EOF past the last one.
func (rd *Reader) Next() (*Block, error) {
	for rd.pos < rd.end {
		h, ok, err := rd.headerAt(rd.pos)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, io.EOF // a file cut short ends with its last whole element
		}
		at := rd.pos
		if !rd.inCluster || (rd.clusterEnd >= 0 && at >= rd.clusterEnd) || (rd.clusterEnd < 0 && topLevel(h.id)) {
			rd.inCluster = false
			if h.id == idCluster {
				rd.inCluster, rd.clusterAt, rd.clusterTS = true, at, 0
				rd.clusterEnd = -1
				if h.size != unknownSize {
					rd.clusterEnd = at + int64(h.hlen) + h.size
				}
				rd.pos = at + int64(h.hlen)
				continue
			}
			if h.size == unknownSize {
				return nil, formatErr("element 0x%x of unknown size at %d", h.id, at)
			}
			rd.pos = at + int64(h.hlen) + h.size // Cues, Tags, Void, … between or after the clusters
			continue
		}
		if h.size == unknownSize {
			return nil, formatErr("cluster child 0x%x of unknown size at %d", h.id, at)
		}
		next := at + int64(h.hlen) + h.size
		if rd.clusterEnd >= 0 && next > rd.clusterEnd {
			return nil, formatErr("element 0x%x at %d overruns its cluster", h.id, at)
		}
		if next > rd.f.Size {
			return nil, io.EOF // a file cut short ends with its last whole element
		}
		switch h.id {
		case idTimestamp:
			if h.size > 8 {
				return nil, formatErr("cluster timestamp of %d bytes at %d", h.size, at)
			}
			b, ok, err := rd.bytesAt(at+int64(h.hlen), h.size)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, io.EOF
			}
			v, err := uintOf(b[:h.size])
			if err != nil {
				return nil, err
			}
			rd.clusterTS = int64(v)
			rd.pos = next
		case idSimpleBlock:
			blk, err := rd.block(at, at+int64(h.hlen), h.size, true)
			rd.pos = next
			if err != nil {
				return nil, err
			}
			if blk != nil {
				return blk, nil
			}
		case idBlockGroup:
			blk, err := rd.group(at, at+int64(h.hlen), h.size)
			rd.pos = next
			if err != nil {
				return nil, err
			}
			if blk != nil {
				return blk, nil
			}
		default:
			rd.pos = next // Position, PrevSize, Void, CRC-32, …
		}
	}
	return nil, io.EOF
}

// blockHead is the most a block's own header takes: the track's VINT (at
// most 8 bytes), the timestamp and the flags.
const blockHead = 11

// block reads the SimpleBlock or Block whose payload of size bytes starts at
// at (its element at elem); nil, nil for a file cut short inside it.
func (rd *Reader) block(elem, at, size int64, simple bool) (*Block, error) {
	if size > MaxBlock {
		return nil, formatErr("block at %d of %d bytes", elem, size)
	}
	b, ok, err := rd.bytesAt(at, min(size, blockHead))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, io.EOF
	}
	track, n, _, err := readVint(b)
	if err != nil {
		return nil, err
	}
	if n == 0 || int64(n)+3 > size {
		return nil, formatErr("block at %d too short", elem)
	}
	rel := int64(int16(uint16(b[n])<<8 | uint16(b[n+1])))
	flags := b[n+2]
	blk := &Block{Track: track, Timestamp: rd.clusterTS + rel, Duration: -1, Pos: elem, Cluster: rd.clusterAt,
		Invisible: flags&0x08 != 0}
	if simple {
		blk.Keyframe = flags&0x80 != 0
		blk.Discardable = flags&0x01 != 0
	}
	if rd.Want != nil && !rd.Want(track) {
		return blk, nil
	}
	data, ok, err := rd.bytesAt(at, size)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, io.EOF
	}
	frames, err := unlace(data[n+3:size], (flags>>1)&3)
	if err != nil {
		return nil, formatErr("block at %d: %v", elem, err)
	}
	if t := rd.f.Track(track); t != nil && len(t.Encodings) > 0 {
		limit := rd.MaxInflated
		if limit <= 0 {
			limit = DefaultMaxInflated
		}
		bound := max(0, min(int64(MaxBlockInflated), limit-rd.inflated))
		budget := bound
		for i, fr := range frames {
			var inflated int64
			if frames[i], inflated, err = t.decode(fr, budget); err != nil {
				if errors.Is(err, errInflated) {
					return nil, formatErr("block at %d: its frames inflate past %d bytes", elem, bound)
				}
				return nil, formatErr("block at %d: %v", elem, err)
			}
			budget -= inflated
			rd.inflated += inflated
		}
	}
	blk.Frames = frames
	return blk, nil
}

// Inflated is how many bytes the frames the Reader returned inflated to.
func (rd *Reader) Inflated() int64 { return rd.inflated }

// group reads a BlockGroup of size bytes from at (its element at elem): its
// Block, its duration, and whether it references another (then it is no
// keyframe).
func (rd *Reader) group(elem, at, size int64) (*Block, error) {
	end := at + size
	if end > rd.f.Size {
		return nil, io.EOF // cut short inside it
	}
	var blk *Block
	keyframe, duration := true, int64(-1)
	for pos := at; pos < end; {
		h, ok, err := rd.headerAt(pos)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, io.EOF
		}
		if h.size == unknownSize || pos+int64(h.hlen)+h.size > end {
			return nil, formatErr("BlockGroup child at %d overruns its group", pos)
		}
		data := pos + int64(h.hlen)
		switch h.id {
		case idBlock:
			if blk, err = rd.block(elem, data, h.size, false); err != nil {
				return nil, err
			}
		case idReferenceBlock:
			keyframe = false
		case idBlockDuration:
			if h.size > 8 {
				return nil, formatErr("BlockDuration of %d bytes at %d", h.size, pos)
			}
			b, ok, err := rd.bytesAt(data, h.size)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, io.EOF
			}
			v, err := uintOf(b[:h.size])
			if err != nil {
				return nil, err
			}
			duration = int64(v)
		}
		pos = data + h.size
	}
	if blk == nil {
		return nil, formatErr("BlockGroup at %d without a Block", elem)
	}
	blk.Keyframe, blk.Duration = keyframe, duration
	return blk, nil
}

// unlace splits a block's frames data by its lacing (0 none, 1 Xiph, 2
// fixed-size, 3 EBML).
func unlace(data []byte, lacing byte) ([][]byte, error) {
	if lacing == 0 {
		return [][]byte{data}, nil
	}
	if len(data) == 0 {
		return nil, formatErr("laced block without a frame count")
	}
	n := int(data[0]) + 1
	data = data[1:]
	sizes := make([]int, n)
	// Every size is checked against the bytes there are as it is read, so no
	// sum of them overflows: a frame is never larger than its block.
	switch lacing {
	case 1: // Xiph: each size but the last as a run of 255s and a final byte
		for i := 0; i < n-1; i++ {
			s := 0
			for {
				if len(data) == 0 {
					return nil, formatErr("Xiph lacing runs out")
				}
				c := data[0]
				data = data[1:]
				s += int(c)
				if s > len(data) {
					return nil, formatErr("Xiph lacing: a frame of %d bytes in %d", s, len(data))
				}
				if c != 255 {
					break
				}
			}
			sizes[i] = s
		}
	case 2: // fixed: every frame the same size
		if len(data)%n != 0 {
			return nil, formatErr("fixed lacing: %d bytes for %d frames", len(data), n)
		}
		for i := range sizes {
			sizes[i] = len(data) / n
		}
	case 3: // EBML: the first size, then signed differences
		v, m, _, err := readVint(data)
		if err != nil || m == 0 {
			return nil, formatErr("EBML lacing: bad first size")
		}
		data = data[m:]
		if v > uint64(len(data)) {
			return nil, formatErr("EBML lacing: a frame of %d bytes in %d", v, len(data))
		}
		sizes[0] = int(v)
		for i := 1; i < n-1; i++ {
			d, m, _, err := readVint(data)
			if err != nil || m == 0 {
				return nil, formatErr("EBML lacing: bad size %d", i)
			}
			data = data[m:]
			s := int64(sizes[i-1]) + int64(d) - (int64(1)<<(7*m-1) - 1)
			if s < 0 || s > int64(len(data)) {
				return nil, formatErr("EBML lacing: a frame of %d bytes in %d", s, len(data))
			}
			sizes[i] = int(s)
		}
	}
	if lacing != 2 {
		used := 0
		for _, s := range sizes[:n-1] {
			used += s
			if used > len(data) {
				return nil, formatErr("laced frames of %d bytes in %d", used, len(data))
			}
		}
		sizes[n-1] = len(data) - used
	}
	frames := make([][]byte, n)
	for i, s := range sizes {
		frames[i] = data[:s:s]
		data = data[s:]
	}
	return frames, nil
}

// Decodable says whether the reader can undo the track's content encodings:
// header stripping and zlib, not encryption or another compression.
func (t *Track) Decodable() bool {
	for _, e := range t.Encodings {
		if e.Scope&1 == 0 {
			continue
		}
		if e.Type != 0 || (e.CompAlgo != compHeaderStripping && e.CompAlgo != compZlib) {
			return false
		}
	}
	return true
}

// errInflated is a frame that inflates past what it may.
var errInflated = errors.New("inflates past its bound")

// decode undoes the track's content encodings on a frame, the highest order
// first, inflating it to budget bytes at most (errInflated past them);
// inflated is how many its zlib layers came to.
func (t *Track) decode(frame []byte, budget int64) (out []byte, inflated int64, err error) {
	for i := len(t.Encodings) - 1; i >= 0; i-- {
		e := t.Encodings[i]
		if e.Scope&1 == 0 {
			continue
		}
		if e.Type != 0 {
			return nil, inflated, formatErr("track %d is encrypted", t.Number)
		}
		switch e.CompAlgo {
		case compHeaderStripping:
			out := make([]byte, 0, len(e.CompSettings)+len(frame))
			frame = append(append(out, e.CompSettings...), frame...)
		case compZlib:
			zr, err := zlib.NewReader(bytes.NewReader(frame))
			if err != nil {
				return nil, inflated, formatErr("track %d: zlib: %v", t.Number, err)
			}
			left := budget - inflated
			if left < 0 {
				left = 0
			}
			out, err := io.ReadAll(io.LimitReader(zr, left+1))
			if err != nil {
				return nil, inflated, formatErr("track %d: zlib: %v", t.Number, err)
			}
			if int64(len(out)) > left {
				return nil, inflated, errInflated
			}
			inflated += int64(len(out))
			frame = out
		default:
			return nil, inflated, formatErr("track %d: compression %d", t.Number, e.CompAlgo)
		}
	}
	return frame, inflated, nil
}
