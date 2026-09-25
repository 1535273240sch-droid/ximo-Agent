// byteencoder.go —— GPT-2 ByteLevel 字节到 Unicode 映射（v1 initByteEncoder 的对等实现）。
//
// 将 0-255 的字节值映射为可打印 Unicode 字符，使任何字节序列都能无损地
// 表示为字符串并参与 BPE 合并。映射表在分词器加载时构造一次，之后只读。
package ctxmgr

import "sync"

// byteEncoder 256 项的字节 → 字符串映射。
//
// 用定长数组替代 map：查表是 O(1) 且无哈希开销，
// 并且避免 map 结构本身的常驻内存（这是每字节都要走的热路径）。
type byteEncoder struct {
	table [256]string
}

var (
	byteEncoderOnce sync.Once
	byteEncoderInst *byteEncoder
)

// newByteEncoder 构造映射表（v1 算法逐行对等）。
//
// 可打印 ASCII(33-126)、Latin-1 片段(161-172, 174-255) 映射到自身；
// 其余字节（控制字符等）按出现顺序映射到 U+0100 起始的字符。
func newByteEncoder() *byteEncoder {
	byteEncoderOnce.Do(func() {
		enc := &byteEncoder{}

		printable := make([]int, 0, 256)
		for i := 33; i <= 126; i++ {
			printable = append(printable, i)
		}
		for i := 161; i <= 172; i++ {
			printable = append(printable, i)
		}
		for i := 174; i <= 255; i++ {
			printable = append(printable, i)
		}

		isPrintable := make(map[int]bool, len(printable))
		for _, b := range printable {
			isPrintable[b] = true
		}

		allBytes := make([]int, 0, 256)
		mappedChars := make([]int, 0, 256)
		for _, b := range printable {
			allBytes = append(allBytes, b)
			mappedChars = append(mappedChars, b)
		}
		n := 0
		for b := 0; b < 256; b++ {
			if !isPrintable[b] {
				allBytes = append(allBytes, b)
				mappedChars = append(mappedChars, 256+n)
				n++
			}
		}

		for i := range allBytes {
			enc.table[allBytes[i]] = string(rune(mappedChars[i]))
		}
		byteEncoderInst = enc
	})
	return byteEncoderInst
}

// encode 查表得到单个字节对应的字符串。
func (e *byteEncoder) encode(b byte) string { return e.table[b] }
