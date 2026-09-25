package git

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// objectStore 提供对 .git/objects 的读取：松散对象 + packfile（含
// OFS_DELTA / REF_DELTA 解析）。只读，无副作用。
type objectStore struct {
	objectsDir string

	mu     sync.Mutex
	packs  []*packFile // 懒加载
	loaded bool
}

// gitObject 一个 git 对象。
type gitObject struct {
	Type string // commit | tree | blob | tag
	Data []byte
}

// newObjectStore 创建对象存储。
func newObjectStore(gitDir string) *objectStore {
	return &objectStore{
		objectsDir: filepath.Join(gitDir, "objects"),
		mu:         sync.Mutex{},
	}
}

// read 按 sha1（hex）读取对象。
func (s *objectStore) read(sha string) (gitObject, error) {
	if obj, err := s.readLoose(sha); err == nil {
		return obj, nil
	}
	if err := s.ensurePacks(); err != nil {
		return gitObject{}, err
	}
	for _, p := range s.packs {
		if obj, ok, err := p.read(sha); err != nil {
			return gitObject{}, err
		} else if ok {
			return obj, nil
		}
	}
	return gitObject{}, fmt.Errorf("对象 %s 不存在", sha)
}

func (s *objectStore) readLoose(sha string) (gitObject, error) {
	if len(sha) != 40 {
		return gitObject{}, fmt.Errorf("sha1 长度非法: %q", sha)
	}
	path := filepath.Join(s.objectsDir, sha[:2], sha[2:])
	f, err := os.Open(path)
	if err != nil {
		return gitObject{}, err
	}
	defer f.Close()
	zr, err := zlib.NewReader(f)
	if err != nil {
		return gitObject{}, err
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, 256<<20))
	if err != nil {
		return gitObject{}, err
	}
	return parseLoose(raw)
}

// parseLoose 解析 "<type> <size>\0<payload>"。
func parseLoose(raw []byte) (gitObject, error) {
	nul := bytes.IndexByte(raw, 0)
	if nul < 0 {
		return gitObject{}, fmt.Errorf("松散对象缺少头部分隔符")
	}
	header := string(raw[:nul])
	parts := strings.Fields(header)
	if len(parts) != 2 {
		return gitObject{}, fmt.Errorf("松散对象头部非法: %q", header)
	}
	return gitObject{Type: parts[0], Data: raw[nul+1:]}, nil
}

// Close 关闭所有 pack 文件句柄（幂等）。仓库视图不再使用时必须调用，
// 否则 Windows 上会锁住 .git/objects/pack 文件。
func (s *objectStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for _, p := range s.packs {
		p.mu.Lock()
		if p.f != nil {
			if err := p.f.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
			p.f = nil
		}
		p.mu.Unlock()
	}
	s.packs = nil
	s.loaded = false
	return firstErr
}

func (s *objectStore) ensurePacks() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return nil
	}
	s.loaded = true
	packDir := filepath.Join(s.objectsDir, "pack")
	entries, err := os.ReadDir(packDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".idx") {
			continue
		}
		base := strings.TrimSuffix(name, ".idx")
		idxPath := filepath.Join(packDir, name)
		packPath := filepath.Join(packDir, base+".pack")
		if _, err := os.Stat(packPath); err != nil {
			continue
		}
		p, err := openPack(idxPath, packPath)
		if err != nil {
			continue // 单个 pack 损坏不影响其他 pack
		}
		// 跨 pack 的 ref-delta 基对象：回退到全局对象查找。
		store := s
		p.lookup = func(sha string) (gitObject, error) {
			return store.readLooseOrPacks(sha, p)
		}
		s.packs = append(s.packs, p)
	}
	return nil
}

// readLooseOrPacks 读取对象，跳过指定的 pack（用于跨 pack 的 ref-delta 基对象）。
func (s *objectStore) readLooseOrPacks(sha string, skip *packFile) (gitObject, error) {
	if obj, err := s.readLoose(sha); err == nil {
		return obj, nil
	}
	s.mu.Lock()
	packs := append([]*packFile(nil), s.packs...)
	s.mu.Unlock()
	for _, p := range packs {
		if p == skip {
			continue
		}
		if obj, ok, err := p.read(sha); err == nil && ok {
			return obj, nil
		}
	}
	return gitObject{}, fmt.Errorf("对象 %s 不存在", sha)
}

// ---------------------------------------------------------------------------
// packfile
// ---------------------------------------------------------------------------

const (
	objCommit   = 1
	objTree     = 2
	objBlob     = 3
	objTag      = 4
	objOfsDelta = 6
	objRefDelta = 7
)

type packFile struct {
	idxPath  string
	packPath string

	mu    sync.Mutex
	f     *os.File
	table map[string]packEntry // sha -> entry
	// lookup 由 objectStore 注入，用于跨 pack 的 ref-delta 基对象查找。
	lookup func(sha string) (gitObject, error)
}

type packEntry struct {
	offset uint64
	crc32  uint32
}

func openPack(idxPath, packPath string) (*packFile, error) {
	idx, err := os.ReadFile(idxPath)
	if err != nil {
		return nil, err
	}
	if len(idx) < 8 || !bytes.Equal(idx[:4], []byte{0xff, 0x74, 0x4f, 0x63}) {
		return nil, fmt.Errorf("pack idx 魔法数非法")
	}
	version := binary.BigEndian.Uint32(idx[4:8])
	if version != 2 {
		return nil, fmt.Errorf("仅支持 pack idx v2，收到 v%d", version)
	}
	fanoutStart := 8
	count := binary.BigEndian.Uint32(idx[fanoutStart+255*4:])
	shaStart := fanoutStart + 256*4
	crcStart := shaStart + int(count)*20
	offStart := crcStart + int(count)*4
	bigStart := offStart + int(count)*4

	table := make(map[string]packEntry, count)
	for i := uint32(0); i < count; i++ {
		sha := hex.EncodeToString(idx[shaStart+int(i)*20 : shaStart+int(i)*20+20])
		crc := binary.BigEndian.Uint32(idx[crcStart+int(i)*4:])
		off := uint64(binary.BigEndian.Uint32(idx[offStart+int(i)*4:]))
		if off&0x80000000 != 0 {
			bigIdx := off & 0x7fffffff
			pos := bigStart + int(bigIdx)*8
			if pos+8 > len(idx) {
				continue
			}
			off = binary.BigEndian.Uint64(idx[pos : pos+8])
		}
		table[sha] = packEntry{offset: off, crc32: crc}
	}
	f, err := os.Open(packPath)
	if err != nil {
		return nil, err
	}
	return &packFile{idxPath: idxPath, packPath: packPath, f: f, table: table}, nil
}

// read 从 pack 中读取对象（必要时解析 delta 链）。
func (p *packFile) read(sha string) (gitObject, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.table[sha]
	if !ok {
		return gitObject{}, false, nil
	}
	obj, err := p.readAt(entry.offset, 0)
	if err != nil {
		return gitObject{}, false, err
	}
	return obj, true, nil
}

// readAt 解析指定偏移处的 pack 条目。depth 限制 delta 链深度（防恶意/损坏包）。
func (p *packFile) readAt(offset uint64, depth int) (gitObject, error) {
	if depth > 64 {
		return gitObject{}, fmt.Errorf("pack delta 链深度超过 64")
	}
	if _, err := p.f.Seek(int64(offset), io.SeekStart); err != nil {
		return gitObject{}, err
	}
	br := io.LimitReader(p.f, 64<<20)

	objType, _, err := readPackVarint(br)
	if err != nil {
		return gitObject{}, err
	}

	switch objType {
	case objCommit, objTree, objBlob, objTag:
		data, err := readZlibData(br)
		if err != nil {
			return gitObject{}, err
		}
		return gitObject{Type: packTypeName(objType), Data: data}, nil
	case objOfsDelta:
		deltaOff, err := readPackOffsetDelta(br)
		if err != nil {
			return gitObject{}, err
		}
		delta, err := readZlibData(br)
		if err != nil {
			return gitObject{}, err
		}
		if offset < deltaOff {
			return gitObject{}, fmt.Errorf("pack ofs-delta 偏移回退")
		}
		base, err := p.readAt(offset-deltaOff, depth+1)
		if err != nil {
			return gitObject{}, err
		}
		return applyPackDelta(base, delta)
	case objRefDelta:
		var baseSha [20]byte
		if _, err := io.ReadFull(br, baseSha[:]); err != nil {
			return gitObject{}, err
		}
		delta, err := readZlibData(br)
		if err != nil {
			return gitObject{}, err
		}
		baseObj, ok, err := p.read(hex.EncodeToString(baseSha[:]))
		if err != nil {
			return gitObject{}, err
		}
		if !ok {
			// 跨 pack 的 ref-delta 基对象：回退到对象存储全局查找。
			baseObj, err = p.lookup(hex.EncodeToString(baseSha[:]))
			if err != nil {
				return gitObject{}, err
			}
		}
		return applyPackDelta(baseObj, delta)
	default:
		return gitObject{}, fmt.Errorf("未知 pack 对象类型 %d", objType)
	}
}

func packTypeName(t uint64) string {
	switch t {
	case objCommit:
		return "commit"
	case objTree:
		return "tree"
	case objBlob:
		return "blob"
	case objTag:
		return "tag"
	}
	return "unknown"
}

// readPackVarint 读取 pack 条目的头部：返回对象类型与（未压缩）数据大小。
func readPackVarint(r io.Reader) (typ uint64, size uint64, err error) {
	var buf [1]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, 0, err
	}
	first := buf[0]
	typ = uint64((first >> 4) & 0x7)
	size = uint64(first & 0x0f)
	shift := uint(4)
	for first&0x80 != 0 {
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return 0, 0, err
		}
		size |= uint64(buf[0]&0x7f) << shift
		shift += 7
		first = buf[0]
	}
	return typ, size, nil
}

// readPackOffsetDelta 读取 ofs-delta 的负偏移（不同于普通 varint 的编码）。
func readPackOffsetDelta(r io.Reader) (uint64, error) {
	var buf [1]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, err
	}
	value := uint64(buf[0] & 0x7f)
	for buf[0]&0x80 != 0 {
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return 0, err
		}
		value = ((value + 1) << 7) | uint64(buf[0]&0x7f)
	}
	return value, nil
}

// readZlibData 从当前位置读取一段 zlib 压缩数据并解压。
func readZlibData(r io.Reader) ([]byte, error) {
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(io.LimitReader(zr, 256<<20))
}

// applyPackDelta 应用 pack delta（copy/insert 指令）。
func applyPackDelta(base gitObject, delta []byte) (gitObject, error) {
	pos := 0
	readVarint := func() (uint64, error) {
		var value uint64
		var shift uint
		for {
			if pos >= len(delta) {
				return 0, fmt.Errorf("delta varint 越界")
			}
			b := delta[pos]
			pos++
			value |= uint64(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value, nil
	}
	baseSize, err := readVarint()
	if err != nil {
		return gitObject{}, err
	}
	resultSize, err := readVarint()
	if err != nil {
		return gitObject{}, err
	}
	if uint64(len(base.Data)) != baseSize {
		return gitObject{}, fmt.Errorf("delta 基大小不匹配（期望 %d，实际 %d）", baseSize, len(base.Data))
	}
	out := make([]byte, 0, resultSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			// copy：从 base 复制一段
			var cpOff, cpSize uint64
			for i := uint(0); i < 4; i++ {
				if op&(1<<i) != 0 {
					if pos >= len(delta) {
						return gitObject{}, fmt.Errorf("delta copy 偏移越界")
					}
					cpOff |= uint64(delta[pos]) << (8 * i)
					pos++
				}
			}
			for i := uint(0); i < 3; i++ {
				if op&(0x10<<i) != 0 {
					if pos >= len(delta) {
						return gitObject{}, fmt.Errorf("delta copy 长度越界")
					}
					cpSize |= uint64(delta[pos]) << (8 * i)
					pos++
				}
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff+cpSize > uint64(len(base.Data)) {
				return gitObject{}, fmt.Errorf("delta copy 范围越界")
			}
			out = append(out, base.Data[cpOff:cpOff+cpSize]...)
		} else if op != 0 {
			// insert：从 delta 复制 op 字节
			if pos+int(op) > len(delta) {
				return gitObject{}, fmt.Errorf("delta insert 越界")
			}
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return gitObject{}, fmt.Errorf("delta 操作码 0 非法")
		}
	}
	if uint64(len(out)) != resultSize {
		return gitObject{}, fmt.Errorf("delta 结果大小不匹配（期望 %d，实际 %d）", resultSize, len(out))
	}
	return gitObject{Type: base.Type, Data: out}, nil
}
