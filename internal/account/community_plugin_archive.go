package account

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
	"unsafe"

	"github.com/pelletier/go-toml/v2"

	"github.com/metasequoiaime/MSIME-Backend/internal/textutil"
)

// 社区插件包的规则与客户端 plugins.rs 和 plugins/import.rs 一致；上传、解压和账号配额另受服务端限制。
const (
	maxPluginArchiveBytes   = 8 << 20
	maxPluginArchiveMembers = 64
	// 与客户端的目录读取预算一致：为结尾记录、最长归档注释及 64 个成员的元数据保留空间。
	maxPluginDirectoryReadBytes = 128<<10 + maxPluginArchiveMembers*(4<<10)
	// The client refuses a pack folder with more regular files than this, the manifest and notices included.
	maxPluginPackFiles     = 16
	maxPluginManifestBytes = 256 << 10
	maxPluginNoticeBytes   = 64 << 10
	maxPluginFileBytes     = 16 << 20
	// The archive itself is at most 8 MiB, so this bounds how much a publish may inflate in total.
	maxPluginUnpackedBytes = 24 << 20
	// Past a small allowance, the declared uncompressed total may be at most this multiple of the archive size. Deflate tops out near 1032:1; audio and text packs stay far below 100:1, while a zip bomb does not.
	maxPluginInflateRatio  = 100
	pluginInflateAllowance = 1 << 20
	// The archive travels as standard base64 inside JSON: 8 MiB encodes to 11,184,812 bytes, and the rest covers the metadata fields.
	maxPluginPublishBytes = 11_300_000
	maxPluginsPerUser     = 20
	// Total stored archive bytes per account.
	maxPluginBytesPerUser  = 32 << 20
	pluginPublishesPerHour = 10
	// Download attempts per account per hour; each one holds up to 8 MiB of archive while it is written out.
	pluginDownloadsPerHour = 60
)

// Per-kind audio bounds, from sound_pack.rs and music_pack.rs.
const (
	maxSoundSamples     = 8
	maxSoundSampleBytes = 512 << 10
	maxSoundPackBytes   = 4 << 20
	maxSoundSemitones   = 128
	maxMusicTracks      = 8
	maxMusicTrackBytes  = 16 << 20
	maxMusicPackBytes   = 64 << 20
	maxCommandRows      = 256
	maxCommandTrigger   = 32
	maxCommandTitle     = 48
	maxCommandTemplate  = 199
)

// Effect pack bounds, from effect_pack.rs.
const (
	maxEffectIntensity = 100
	maxEffectColors    = 4
	minEffectDuration  = 60
	maxEffectDuration  = 1500
	maxEffectParticles = 64
)

// 新类型的数据边界，与客户端 plugins/{phrase_table,helpcode_pack,wordbook_pack,symbol_set}.rs 一致。
const (
	// readPluginArchive 对不超过这个大小的 `.txt`/`.tsv` 成员保留完整字节，供数据文件的内容校验使用；单个数据文件的上限都不超过它，总量仍受 24 MiB 解压上限约束。
	maxPluginDataBytes = 4 << 20

	maxPhraseRows            = 2000
	maxPhraseKey             = 32
	maxPhraseText            = 199
	maxHelpcodeBytes         = 1 << 20
	maxHelpcodeEntries       = 30000
	maxWordbookBytes         = 4 << 20
	maxWordbookEntries       = 20000
	maxWordbookWordChars     = 64
	maxWordbookPhoneticChars = 64
	maxWordbookMeaningChars  = 256
	maxWordbookNameChars     = 64
	// 客户端把单词本插件的书 id 记为 `pack-<id>`，书 id 最长 64 个字符，所以插件 id 最长 59 个字符。
	maxWordbookIDChars    = 59
	maxSymbolGroups       = 32
	maxSymbolGroupItems   = 512
	maxSymbolItems        = 2048
	maxSymbolTitleBytes   = 48
	maxSymbolKeywordBytes = 256
	maxSymbolItemUnits    = 64
)

// pluginKinds 是可发布类型的白名单，即客户端的 PluginKind::ALL。
var pluginKinds = []string{"sound", "music", "command_table", "effect", "helpcode", "symbol_set", "phrase_table", "wordbook"}

// legacyPluginKinds 是不支持 `kinds` 声明的旧客户端能识别的全部类型，列表和详情在客户端没有声明时只返回这些类型。它是冻结的：以后新增的类型只加进 pluginKinds，不加进这里，否则旧客户端会收到不认识的类型。
var legacyPluginKinds = []string{"sound", "music", "command_table", "effect"}

// builtinPluginIDs are the ids the client bundles for a kind; it refuses to import a pack over one of them.
var builtinPluginIDs = map[string][]string{
	"sound": {"default", "twinkle", "msime-typewriter", "msime-bubble", "msime-8bit", "msime-woodblock", "msime-pentatonic", "msime-canon", "msime-ode-to-joy"},
	"music": {"msime-music-lofi", "msime-music-ambient"},
}

// pluginPack is what a validated archive yields for storage and listing.
type pluginPack struct {
	Kind     string
	ID       string
	Name     string
	Version  string
	License  string
	Manifest []byte
}

// validPluginID 沿用客户端的标识符形状，并拒绝 Windows 无法按原名安装的尾点与 nul 目录。
func validPluginID(id string) bool {
	if id == "" || len(id) > 64 || !isASCIIAlnum(id[0]) || id == "nul" || strings.HasSuffix(id, ".") {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func isASCIIAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// validPluginFileName 与客户端 valid_file_name 一致：单个文件名至多 64 个 ASCII 字母、数字、点、横线和下划线，以字母或数字开头，并拒绝客户端列出的设备保留名。
func validPluginFileName(name string) bool {
	if name == "" || len(name) > 64 || !isASCIIAlnum(name[0]) || name == "." || name == ".." {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(isASCIIAlnum(c) || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	// 字符白名单已保证名称是 ASCII；直接比较保留名，避免复制整个名称。
	stem, _, _ := strings.Cut(name, ".")
	if len(stem) == 3 && (strings.EqualFold(stem, "con") || strings.EqualFold(stem, "prn") || strings.EqualFold(stem, "aux") || strings.EqualFold(stem, "nul")) {
		return false
	}
	if len(stem) == 4 && (strings.EqualFold(stem[:3], "com") || strings.EqualFold(stem[:3], "lpt")) && stem[3] >= '0' && stem[3] <= '9' {
		return false
	}
	return true
}

func pluginExtension(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return ""
	}
	ext := name[i+1:]
	upper, nonASCII := false, false
	for i := range ext {
		if ext[i] >= utf8.RuneSelf {
			nonASCII = true
			break
		}
		if ext[i] >= 'A' && ext[i] <= 'Z' {
			upper = true
			break
		}
	}
	if !upper && !nonASCII {
		return ext
	}
	if known, ok := knownPluginExtension(ext); ok {
		return known
	}
	return strings.ToLower(ext)
}

func knownPluginExtension(ext string) (string, bool) {
	fold := func(c byte) byte {
		if c >= 'A' && c <= 'Z' {
			return c + ('a' - 'A')
		}
		return c
	}
	if len(ext) == 2 {
		switch fold(ext[0]) {
		case '7':
			if textutil.EqualFoldASCII(ext, "7z") {
				return "7z", true
			}
		case 'g':
			if textutil.EqualFoldASCII(ext, "gz") {
				return "gz", true
			}
		case 'l':
			if textutil.EqualFoldASCII(ext, "lz") {
				return "lz", true
			}
		case 'm':
			if textutil.EqualFoldASCII(ext, "md") {
				return "md", true
			}
		case 'x':
			if textutil.EqualFoldASCII(ext, "xz") {
				return "xz", true
			}
		}
		return "", false
	}
	if len(ext) == 4 && textutil.EqualFoldASCII(ext, "lzma") {
		return "lzma", true
	}
	if len(ext) != 3 {
		return "", false
	}
	switch fold(ext[0]) {
	case 'a':
		if textutil.EqualFoldASCII(ext, "apk") {
			return "apk", true
		}
	case 'b':
		if textutil.EqualFoldASCII(ext, "bz2") {
			return "bz2", true
		}
	case 'c':
		if textutil.EqualFoldASCII(ext, "cab") {
			return "cab", true
		}
	case 'j':
		if textutil.EqualFoldASCII(ext, "jar") {
			return "jar", true
		}
	case 'o':
		if textutil.EqualFoldASCII(ext, "ogg") {
			return "ogg", true
		}
	case 'r':
		if textutil.EqualFoldASCII(ext, "rar") {
			return "rar", true
		}
	case 't':
		for _, known := range [...]string{"tar", "tgz", "txt", "tsv"} {
			if textutil.EqualFoldASCII(ext, known) {
				return known, true
			}
		}
	case 'w':
		if textutil.EqualFoldASCII(ext, "wav") {
			return "wav", true
		}
	case 'z':
		if textutil.EqualFoldASCII(ext, "zst") {
			return "zst", true
		}
		if textutil.EqualFoldASCII(ext, "zip") {
			return "zip", true
		}
	}
	return "", false
}

func pluginAudio(name string) bool {
	ext := pluginExtension(name)
	return ext == "wav" || ext == "ogg"
}

// pluginDataExtension 报告成员是否可能是数据文件（`.txt`/`.tsv`），这类成员的完整字节会被保留下来。
func pluginDataExtension(name string) bool {
	ext := pluginExtension(name)
	return ext == "txt" || ext == "tsv"
}

func pluginNotice(name string) bool {
	ext := pluginExtension(name)
	return ext == "txt" || ext == "md"
}

// pluginNestedArchive reports a member whose name or leading bytes belong to an archive or compressed stream. Packs carry only TOML, audio and text, so any such member is refused rather than inspected.
func pluginNestedArchive(name string, head []byte) bool {
	switch pluginExtension(name) {
	case "zip", "jar", "apk", "7z", "rar", "tar", "gz", "tgz", "bz2", "xz", "zst", "lz", "lzma", "cab":
		return true
	}
	for _, magic := range []string{"PK\x03\x04", "PK\x05\x06", "PK\x07\x08", "\x1f\x8b", "BZh", "\xfd7zXZ\x00", "7z\xbc\xaf\x27\x1c", "Rar!\x1a\x07", "\x28\xb5\x2f\xfd", "MSCF"} {
		if bytes.HasPrefix(head, []byte(magic)) {
			return true
		}
	}
	return len(head) >= 262 && string(head[257:262]) == "ustar"
}

type pluginArchivePathInfo struct {
	base   string
	depth  int
	hidden bool
	dir    bool
}

// pluginArchivePath 检查成员路径并收集遍历所需的信息，不为路径段分配切片。
// 绝对路径、盘符、反斜杠、控制字符、空段及 .、.. 段均拒绝；隐藏成员同样检查完整路径。
func pluginArchivePath(name string) (pluginArchivePathInfo, bool) {
	if name == "" || !utf8.ValidString(name) || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:") || strings.ContainsFunc(name, unicode.IsControl) {
		return pluginArchivePathInfo{}, false
	}
	info := pluginArchivePathInfo{dir: strings.HasSuffix(name, "/")}
	remaining := strings.TrimSuffix(name, "/")
	for {
		part, rest, more := strings.Cut(remaining, "/")
		if part == "" || part == "." || part == ".." {
			return pluginArchivePathInfo{}, false
		}
		if strings.HasPrefix(part, ".") || info.depth == 0 && part == "__MACOSX" {
			info.hidden = true
		}
		info.base = part
		info.depth++
		if !more {
			return info, true
		}
		remaining = rest
	}
}

// pluginMember is one pack file after the archive walk: its bytes are read only as far as the checks need, the manifest in full.
type pluginMember struct {
	size int
	head []byte
	data []byte
}

var errPluginDirectoryTooLarge = errors.New("plugin archive directory exceeds read budget")

// pluginDirectoryReader 在解析目录期间限制累计读取量，避免标准库在成员数检查前解析全部成员。
// 校验通过后解除预算，文件内容仍由各自的大小上限约束；整个归档校验按顺序读取。
type pluginDirectoryReader struct {
	*bytes.Reader
	remaining int64
}

func (r *pluginDirectoryReader) ReadAt(p []byte, offset int64) (int, error) {
	if r.remaining >= 0 && int64(len(p)) > r.remaining {
		return 0, errPluginDirectoryTooLarge
	}
	n, err := r.Reader.ReadAt(p, offset)
	if r.remaining >= 0 {
		r.remaining -= int64(n)
	}
	return n, err
}

// readPluginArchive 按客户端导入规则返回插件文件。隐藏成员也计入成员数并检查路径。
// 目录解析受读取预算限制，各文件按声明大小解压一次；伪造长度或 CRC 不符均拒绝。
func readPluginArchive(archive []byte) (map[string]pluginMember, string) {
	if len(archive) < 1 || len(archive) > maxPluginArchiveBytes {
		return nil, "plugin_too_large"
	}
	directory := &pluginDirectoryReader{Reader: bytes.NewReader(archive), remaining: maxPluginDirectoryReadBytes}
	reader, err := zip.NewReader(directory, int64(len(archive)))
	if errors.Is(err, errPluginDirectoryTooLarge) {
		return nil, "plugin_too_large"
	}
	if err != nil {
		return nil, "invalid_plugin_archive"
	}
	if len(reader.File) > maxPluginArchiveMembers {
		return nil, "plugin_too_large"
	}
	directory.remaining = -1
	entries := []*zip.File{}
	for _, f := range reader.File {
		path, ok := pluginArchivePath(f.Name)
		if !ok {
			return nil, "invalid_plugin_archive"
		}
		mode := f.Mode()
		if mode&fs.ModeSymlink != 0 || mode.Type()&^fs.ModeDir != 0 || f.Flags&0x1 != 0 {
			return nil, "invalid_plugin_archive"
		}
		if pluginNestedArchive(path.base, nil) {
			return nil, "invalid_plugin_archive"
		}
		if path.hidden {
			continue
		}
		if path.dir || mode.IsDir() {
			if path.depth > 1 {
				return nil, "invalid_plugin_archive"
			}
			continue
		}
		entries = append(entries, f)
	}
	// 与客户端一致，由第一个文件决定是否使用单个外层文件夹。
	wrapper := ""
	if len(entries) > 0 {
		parent, base, wrapped := strings.Cut(entries[0].Name, "/")
		if wrapped && !strings.Contains(base, "/") {
			wrapper = parent
		}
	}
	if len(entries) > maxPluginPackFiles {
		return nil, "plugin_too_large"
	}
	members := map[string]pluginMember{}
	lowered := map[string]bool{}
	var declared uint64
	for _, file := range entries {
		parent, name, wrapped := strings.Cut(file.Name, "/")
		switch {
		case wrapper == "" && !wrapped:
			name = parent
		case wrapper != "" && wrapped && parent == wrapper && !strings.Contains(name, "/"):
		default:
			return nil, "invalid_plugin_archive"
		}
		// 不区分大小写的文件系统会合并仅大小写不同的文件名。
		if !validPluginFileName(name) {
			return nil, "invalid_plugin_archive"
		}
		loweredName := strings.ToLower(name)
		if lowered[loweredName] {
			return nil, "invalid_plugin_archive"
		}
		lowered[loweredName] = true
		if file.Method != zip.Store && file.Method != zip.Deflate {
			return nil, "invalid_plugin_archive"
		}
		size := file.UncompressedSize64
		declared += size
		if size > maxPluginFileBytes || declared > maxPluginUnpackedBytes || declared > pluginInflateAllowance+maxPluginInflateRatio*uint64(len(archive)) {
			return nil, "plugin_too_large"
		}
		members[name] = pluginMember{size: int(size)}
	}
	for _, file := range entries {
		name := file.Name[strings.LastIndexByte(file.Name, '/')+1:]
		m := members[name]
		r, err := file.Open()
		if err != nil {
			return nil, "invalid_plugin_archive"
		}
		limited := io.LimitReader(r, int64(m.size)+1)
		keepData := name == "plugin.toml" && m.size <= maxPluginManifestBytes || pluginDataExtension(name) && m.size <= maxPluginDataBytes
		bufferSize := 512
		if keepData {
			// 大小已受成员与总量上限约束；多读一个字节以完成 CRC 校验并拒绝伪造长度。
			bufferSize = m.size + 1
		}
		buffer := make([]byte, bufferSize)
		n, err := io.ReadFull(limited, buffer)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			r.Close()
			return nil, "invalid_plugin_archive"
		}
		m.head = buffer[:min(n, 512)]
		read := int64(n)
		if keepData {
			m.data = buffer[:n]
		}
		// 再读至 EOF，区分 ReadFull 生成的短读错误和底层解压或数据描述符的截断错误。
		copied, err := io.Copy(io.Discard, limited)
		if err != nil {
			r.Close()
			return nil, "invalid_plugin_archive"
		}
		read += copied
		r.Close()
		if read != int64(m.size) {
			return nil, "invalid_plugin_archive"
		}
		if pluginNestedArchive(name, m.head) {
			return nil, "invalid_plugin_archive"
		}
		members[name] = m
	}
	return members, ""
}

// pluginKeys reports whether every key of table is one of allowed. It compares exactly, because go-toml matches struct fields case-insensitively and the client treats "Kind" as an unknown key.
func pluginKeys(table map[string]any, allowed ...string) bool {
	for key := range table {
		if !slices.Contains(allowed, key) {
			return false
		}
	}
	return true
}

// pluginBoundedText is the client's is_bounded_text for a required field: not blank, at most max bytes, no control characters at all.
func pluginBoundedText(s string, max int) bool {
	return strings.TrimSpace(s) != "" && len(s) <= max && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// pluginManifest is the typed view of plugin.toml. Pointers tell an absent key from a zero value.
type pluginManifest struct {
	SchemaVersion *int64  `toml:"schema_version"`
	Kind          *string `toml:"kind"`
	ID            *string `toml:"id"`
	Name          *string `toml:"name"`
	Version       *string `toml:"version"`
	License       *string `toml:"license"`
	Author        *string `toml:"author"`
	Description   *string `toml:"description"`
	Permissions   *[]any  `toml:"permissions"`
	Mode          *string `toml:"mode"`
	Sounds        *struct {
		Default     *string `toml:"default"`
		Space       *string `toml:"space"`
		Enter       *string `toml:"enter"`
		Backspace   *string `toml:"backspace"`
		Commit      *string `toml:"commit"`
		Achievement *string `toml:"achievement"`
	} `toml:"sounds"`
	Sequence *struct {
		Sample    *string  `toml:"sample"`
		Semitones *[]int64 `toml:"semitones"`
		Advance   *string  `toml:"advance"`
	} `toml:"sequence"`
	Music *struct {
		Tracks *[]string `toml:"tracks"`
	} `toml:"music"`
	Commands *[]struct {
		Trigger  *string `toml:"trigger"`
		Title    *string `toml:"title"`
		Template *string `toml:"template"`
	} `toml:"commands"`
	Effect *struct {
		Style      *string   `toml:"style"`
		Intensity  *int64    `toml:"intensity"`
		Colors     *[]string `toml:"colors"`
		DurationMS *int64    `toml:"duration_ms"`
		Particles  *int64    `toml:"particles"`
	} `toml:"effect"`
	Phrases *[]struct {
		Key  *string `toml:"key"`
		Text *string `toml:"text"`
	} `toml:"phrases"`
	Helpcode *struct {
		Table *string `toml:"table"`
	} `toml:"helpcode"`
	Wordbook *struct {
		File *string `toml:"file"`
	} `toml:"wordbook"`
	Groups *[]struct {
		Tab      *string   `toml:"tab"`
		Title    *string   `toml:"title"`
		Keywords *string   `toml:"keywords"`
		Items    *[]string `toml:"items"`
	} `toml:"groups"`
}

var pluginCommonKeys = []string{"schema_version", "kind", "id", "name", "version", "license", "author", "description", "permissions"}

var pluginKindKeys = map[string][]string{
	"sound":         {"mode", "sounds", "sequence"},
	"music":         {"music"},
	"command_table": {"commands"},
	"effect":        {"effect"},
	"helpcode":      {"helpcode"},
	"symbol_set":    {"groups"},
	"phrase_table":  {"phrases"},
	"wordbook":      {"wordbook"},
}

// pluginManifestKeys 精确校验公共键与当前类型的专属键，直接读取两份白名单，避免每次校验时合并切片。
func pluginManifestKeys(table map[string]any, kind string) bool {
	kindKeys := pluginKindKeys[kind]
	for key := range table {
		if !slices.Contains(pluginCommonKeys, key) && !slices.Contains(kindKeys, key) {
			return false
		}
	}
	return true
}

// validPluginArchive runs every server-side check on an uploaded pack and returns what is stored, or the error code. It never decodes audio beyond its magic bytes and never executes anything.
func validPluginArchive(archive []byte) (pluginPack, string) {
	members, code := readPluginArchive(archive)
	if code != "" {
		return pluginPack{}, code
	}
	manifestFile, ok := members["plugin.toml"]
	if !ok || manifestFile.size > maxPluginManifestBytes {
		return pluginPack{}, "invalid_plugin_manifest"
	}
	raw := manifestFile.data
	if !utf8.Valid(raw) {
		return pluginPack{}, "invalid_plugin_manifest"
	}
	var table map[string]any
	if toml.Unmarshal(raw, &table) != nil {
		return pluginPack{}, "invalid_plugin_manifest"
	}
	var m pluginManifest
	decoder := toml.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&m) != nil {
		return pluginPack{}, "invalid_plugin_manifest"
	}
	if m.SchemaVersion == nil || *m.SchemaVersion != 1 || m.Kind == nil || !slices.Contains(pluginKinds, *m.Kind) {
		return pluginPack{}, "invalid_plugin_manifest"
	}
	kind := *m.Kind
	if !pluginManifestKeys(table, kind) {
		return pluginPack{}, "invalid_plugin_manifest"
	}
	if m.ID == nil || !validPluginID(*m.ID) || slices.Contains(builtinPluginIDs[kind], *m.ID) {
		return pluginPack{}, "invalid_plugin_manifest"
	}
	if m.Name == nil || !pluginBoundedText(*m.Name, 80) || m.Version == nil || !pluginBoundedText(*m.Version, 32) || m.License == nil || !pluginBoundedText(*m.License, 64) {
		return pluginPack{}, "invalid_plugin_manifest"
	}
	for _, c := range *m.License {
		if !(c < utf8.RuneSelf && isASCIIAlnum(byte(c)) || strings.ContainsRune(".-+() ", c)) {
			return pluginPack{}, "invalid_plugin_manifest"
		}
	}
	if m.Author != nil && !pluginBoundedText(*m.Author, 120) || m.Description != nil && !pluginBoundedText(*m.Description, 500) {
		return pluginPack{}, "invalid_plugin_manifest"
	}
	if m.Permissions != nil && len(*m.Permissions) > 0 {
		return pluginPack{}, "invalid_plugin_manifest"
	}
	var audio []string
	// data 是清单点名的数据文件：文件名到大小上限。
	var data map[string]int
	var fileLimit, totalLimit, countLimit int
	switch kind {
	case "sound":
		audio, ok = validPluginSound(table, m)
		fileLimit, totalLimit, countLimit = maxSoundSampleBytes, maxSoundPackBytes, maxSoundSamples
	case "music":
		audio, ok = validPluginMusic(table, m)
		fileLimit, totalLimit, countLimit = maxMusicTrackBytes, maxMusicPackBytes, maxMusicTracks
	case "command_table":
		ok = validPluginCommands(table, m)
	case "effect":
		ok = validPluginEffect(table, m)
	case "phrase_table":
		ok = validPluginPhrases(table, m)
	case "helpcode":
		data, ok = validPluginHelpcode(table, m)
	case "wordbook":
		data, ok = validPluginWordbook(table, m)
	case "symbol_set":
		ok = validPluginSymbols(table, m)
	}
	if !ok {
		return pluginPack{}, "invalid_plugin_manifest"
	}
	if code := validPluginFiles(members, audio, data, fileLimit, totalLimit, countLimit); code != "" {
		return pluginPack{}, code
	}
	// 数据文件的存在和大小已经检查过，这里按该类型的语法校验内容。
	switch kind {
	case "helpcode":
		ok = validHelpcodeTable(members[*m.Helpcode.Table].data)
	case "wordbook":
		ok = validWordbookEntries(members[*m.Wordbook.File].data)
	}
	if !ok {
		return pluginPack{}, "invalid_plugin_manifest"
	}
	return pluginPack{Kind: kind, ID: *m.ID, Name: *m.Name, Version: *m.Version, License: *m.License, Manifest: raw}, ""
}

// validPluginFiles 对应客户端的 check_files：清单点名的音频文件必须存在、非空、在该类型的上限内且文件头与格式相符；清单点名的数据文件（data，文件名到上限）必须存在、非空且不超过上限，内容由各类型自己校验；其余文件只能是清单或小的说明文件。
func validPluginFiles(members map[string]pluginMember, audio []string, data map[string]int, fileLimit, totalLimit, countLimit int) string {
	distinct := slices.Clone(audio)
	slices.Sort(distinct)
	distinct = slices.Compact(distinct)
	if len(distinct) > countLimit {
		return "invalid_plugin_manifest"
	}
	total := 0
	for _, name := range distinct {
		m, ok := members[name]
		if !ok || m.size == 0 {
			return "invalid_plugin_manifest"
		}
		if m.size > fileLimit {
			return "plugin_too_large"
		}
		total += m.size
		head := m.head
		switch pluginExtension(name) {
		case "wav":
			ok = len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WAVE"
		case "ogg":
			// 客户端对两种音频都先读满 12 字节；只有 OggS 签名的截断文件不能安装。
			ok = len(head) >= 12 && string(head[:4]) == "OggS"
		default:
			ok = false
		}
		if !ok {
			return "invalid_plugin_manifest"
		}
	}
	if total > totalLimit {
		return "plugin_too_large"
	}
	for name, limit := range data {
		m, ok := members[name]
		if !ok || m.size == 0 {
			return "invalid_plugin_manifest"
		}
		if m.size > limit {
			return "plugin_too_large"
		}
	}
	for name, m := range members {
		if _, named := data[name]; name == "plugin.toml" || named || slices.Contains(distinct, name) {
			continue
		}
		if !pluginNotice(name) {
			return "invalid_plugin_manifest"
		}
		if m.size > maxPluginNoticeBytes {
			return "plugin_too_large"
		}
	}
	return ""
}

func pluginAudioName(name *string) bool {
	return name != nil && validPluginFileName(*name) && pluginAudio(*name)
}

// pluginSampleName limits sound samples to WAV: hosts decode a sample whole before playing it, and only a WAV's length can be checked up front (HarmonyOS silences Ogg samples for that reason). Ogg stays allowed for music tracks.
func pluginSampleName(name *string) bool {
	return pluginAudioName(name) && pluginExtension(*name) == "wav"
}

// validPluginSound follows sound_pack.rs: keys mode needs sounds.default and no sequence; sequence mode needs a sequence of 1 to 128 semitones in -24..24.
func validPluginSound(table map[string]any, m pluginManifest) ([]string, bool) {
	mode := "keys"
	if m.Mode != nil {
		mode = *m.Mode
	}
	if mode != "keys" && mode != "sequence" {
		return nil, false
	}
	var audio []string
	if m.Sounds != nil {
		sounds, _ := table["sounds"].(map[string]any)
		if !pluginKeys(sounds, "default", "space", "enter", "backspace", "commit", "achievement") {
			return nil, false
		}
		for _, name := range []*string{m.Sounds.Default, m.Sounds.Space, m.Sounds.Enter, m.Sounds.Backspace, m.Sounds.Commit, m.Sounds.Achievement} {
			if name == nil {
				continue
			}
			if !pluginSampleName(name) {
				return nil, false
			}
			audio = append(audio, *name)
		}
	}
	if m.Sequence != nil {
		sequence, _ := table["sequence"].(map[string]any)
		if !pluginKeys(sequence, "sample", "semitones", "advance") || !pluginSampleName(m.Sequence.Sample) || m.Sequence.Semitones == nil {
			return nil, false
		}
		semitones := *m.Sequence.Semitones
		if len(semitones) < 1 || len(semitones) > maxSoundSemitones {
			return nil, false
		}
		for _, s := range semitones {
			if s < -24 || s > 24 {
				return nil, false
			}
		}
		if m.Sequence.Advance != nil && *m.Sequence.Advance != "key" && *m.Sequence.Advance != "commit" {
			return nil, false
		}
		audio = append(audio, *m.Sequence.Sample)
	}
	switch {
	case mode == "keys" && (m.Sounds == nil || m.Sounds.Default == nil):
		return nil, false
	case mode == "keys" && m.Sequence != nil:
		return nil, false
	case mode == "sequence" && m.Sequence == nil:
		return nil, false
	}
	return audio, true
}

// validPluginMusic follows music_pack.rs: 1 to 8 distinct tracks.
func validPluginMusic(table map[string]any, m pluginManifest) ([]string, bool) {
	music, _ := table["music"].(map[string]any)
	if m.Music == nil || !pluginKeys(music, "tracks") || m.Music.Tracks == nil {
		return nil, false
	}
	tracks := *m.Music.Tracks
	if len(tracks) < 1 || len(tracks) > maxMusicTracks {
		return nil, false
	}
	for i, track := range tracks {
		if !pluginAudioName(&track) || slices.Contains(tracks[:i], track) {
			return nil, false
		}
	}
	return slices.Clone(tracks), true
}

// validPluginCommands follows command_table.rs: 1 to 256 rows with unique lowercase triggers, a short title and a template whose placeholders are {date}, {time}, {weekday}, {date:FMT} or {time:FMT}, and whose expansion stays within the client's bounds.
func validPluginCommands(table map[string]any, m pluginManifest) bool {
	rows, _ := table["commands"].([]any)
	if m.Commands == nil || len(*m.Commands) < 1 || len(*m.Commands) > maxCommandRows || len(rows) != len(*m.Commands) {
		return false
	}
	triggers := map[string]bool{}
	for i, row := range *m.Commands {
		raw, _ := rows[i].(map[string]any)
		if !pluginKeys(raw, "trigger", "title", "template") || row.Trigger == nil || row.Title == nil || row.Template == nil {
			return false
		}
		trigger := *row.Trigger
		if trigger == "" || len(trigger) > maxCommandTrigger || strings.ContainsFunc(trigger, func(c rune) bool { return c < 'a' || c > 'z' }) || triggers[trigger] {
			return false
		}
		triggers[trigger] = true
		if !pluginBoundedText(*row.Title, maxCommandTitle) {
			return false
		}
		template := *row.Template
		if strings.TrimSpace(template) == "" || textutil.UTF16Length(template) > maxCommandTemplate || strings.ContainsFunc(template, unicode.IsControl) || !validCommandTemplate(template) {
			return false
		}
	}
	return true
}

// validPluginEffect follows effect_pack.rs: a built-in style and optional hints within fixed bounds. The pack names no files, so validPluginFiles then admits only the manifest and notices.
func validPluginEffect(table map[string]any, m pluginManifest) bool {
	effect, _ := table["effect"].(map[string]any)
	if m.Effect == nil || !pluginKeys(effect, "style", "intensity", "colors", "duration_ms", "particles") || m.Effect.Style == nil {
		return false
	}
	e := m.Effect
	if !slices.Contains([]string{"flash", "sparks", "power_mode"}, *e.Style) {
		return false
	}
	inRange := func(v *int64, low, high int64) bool { return v == nil || *v >= low && *v <= high }
	if !inRange(e.Intensity, 0, maxEffectIntensity) || !inRange(e.DurationMS, minEffectDuration, maxEffectDuration) || !inRange(e.Particles, 0, maxEffectParticles) {
		return false
	}
	if e.Colors != nil {
		colors := *e.Colors
		if len(colors) < 1 || len(colors) > maxEffectColors {
			return false
		}
		for _, c := range colors {
			if !validEffectColor(c) {
				return false
			}
		}
	}
	return true
}

// pluginLowercase 报告 s 是否只由小写 ASCII 字母组成。
func pluginLowercase(s string) bool {
	return !strings.ContainsFunc(s, func(c rune) bool { return c < 'a' || c > 'z' })
}

// pluginShortText 是短语文本和符号的共同规则：不是空白、不含控制字符、1 到 max 个 UTF-16 单元。
func pluginShortText(s string, max int) bool {
	return strings.TrimSpace(s) != "" && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl) && textutil.UTF16Length(s) <= max
}

// validPluginPhrases 对应 phrase_table.rs：1 到 2000 个 `[[phrases]]`，每行只有 `key`（1 到 32 个小写 ASCII 字母，K 模式的要求）和 `text`；(`key`,`text`) 不能重复，同一个 key 可以对应多条 text。短语表没有数据文件。
func validPluginPhrases(table map[string]any, m pluginManifest) bool {
	rows, _ := table["phrases"].([]any)
	if m.Phrases == nil || len(*m.Phrases) < 1 || len(*m.Phrases) > maxPhraseRows || len(rows) != len(*m.Phrases) {
		return false
	}
	seen := map[[2]string]bool{}
	for i, row := range *m.Phrases {
		raw, _ := rows[i].(map[string]any)
		if !pluginKeys(raw, "key", "text") || row.Key == nil || row.Text == nil {
			return false
		}
		key, text := *row.Key, *row.Text
		if key == "" || len(key) > maxPhraseKey || !pluginLowercase(key) || !pluginShortText(text, maxPhraseText) || seen[[2]string{key, text}] {
			return false
		}
		seen[[2]string{key, text}] = true
	}
	return true
}

// pluginDataName 报告 name 是否是可以点名的数据文件名：合法的包内文件名，扩展名（不区分大小写）为 ext。
func pluginDataName(name *string, ext string) bool {
	return name != nil && validPluginFileName(*name) && pluginExtension(*name) == ext
}

// validPluginHelpcode 对应 helpcode_pack.rs 的清单部分：`[helpcode]` 只有一个键 `table`，点名一个 `.txt` 数据文件，上限 1 MiB（Engine 的 MAX_HELPCODE_BYTES）。
func validPluginHelpcode(table map[string]any, m pluginManifest) (map[string]int, bool) {
	helpcode, _ := table["helpcode"].(map[string]any)
	if m.Helpcode == nil || !pluginKeys(helpcode, "table") || !pluginDataName(m.Helpcode.Table, "txt") {
		return nil, false
	}
	return map[string]int{*m.Helpcode.Table: maxHelpcodeBytes}, true
}

// validPluginWordbook 对应 wordbook_pack.rs 的清单部分：`[wordbook]` 只有一个键 `file`，点名一个 `.tsv` 数据文件，上限 4 MiB。插件 id 还要满足 wordbook::id_is_well_formed（只有小写字母、数字和 `-`，首尾不是 `-`）且不超过 59 个字符，名称不超过 64 个字符（MAX_NAME_CHARS）。
func validPluginWordbook(table map[string]any, m pluginManifest) (map[string]int, bool) {
	wordbook, _ := table["wordbook"].(map[string]any)
	if m.Wordbook == nil || !pluginKeys(wordbook, "file") || !pluginDataName(m.Wordbook.File, "tsv") {
		return nil, false
	}
	id := *m.ID
	if len(id) > maxWordbookIDChars || strings.HasPrefix(id, "-") || strings.HasSuffix(id, "-") || strings.ContainsFunc(id, func(c rune) bool { return !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') }) {
		return nil, false
	}
	if utf8.RuneCountInString(*m.Name) > maxWordbookNameChars {
		return nil, false
	}
	return map[string]int{*m.Wordbook.File: maxWordbookBytes}, true
}

// pluginDataText 校验完整文件为 UTF-8，并剥除开头可选的 BOM；校验函数只读字符串，所以直接借用归档缓冲，避免复制整份数据。
func pluginDataText(raw []byte) (string, bool) {
	if !utf8.Valid(raw) {
		return "", false
	}
	if bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) {
		raw = raw[3:]
	}
	if len(raw) == 0 {
		return "", true
	}
	return unsafe.String(unsafe.SliceData(raw), len(raw)), true
}

// pluginDataLineCapacity 按物理行数预估去重表容量；超过条目上限时不预分配，避免稀疏文本放大内存。
func pluginDataLineCapacity(text string, maxEntries int) int {
	capacity := strings.Count(text, "\n")
	if text != "" && text[len(text)-1] != '\n' {
		capacity++
	}
	if capacity > maxEntries {
		return 0
	}
	return capacity
}

// pluginDataLines 按需跳过空行和注释，并去掉每行末尾的一个 CR，包括没有 LF 的最后一行。
func pluginDataLines(text string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for remaining := text; remaining != ""; {
			remaining = strings.TrimLeft(remaining, "\n")
			if remaining == "" {
				return
			}
			line, rest, _ := strings.Cut(remaining, "\n")
			remaining = rest
			line = strings.TrimSuffix(line, "\r")
			if line != "" && !strings.HasPrefix(line, "#") && !yield(line) {
				return
			}
		}
	}
}

// validHelpcodeTable 是辅助码表的严格语法：每行为空行、`#` 开头的注释或 `<字>=<码>`。`<字>` 恰好是一个非 ASCII、非空白、非控制字符的 Unicode 标量，`<码>` 恰好是 1 到 2 个小写 ASCII 字母（更长的拒绝，不像 Engine 的宽松解析器那样截断），`=` 两侧不允许空格；1 到 30000 条，同一个字出现两次拒绝。
func validHelpcodeTable(raw []byte) bool {
	text, ok := pluginDataText(raw)
	if !ok {
		return false
	}
	seen := make(map[string]bool, pluginDataLineCapacity(text, maxHelpcodeEntries))
	for line := range pluginDataLines(text) {
		char, code, found := strings.Cut(line, "=")
		r, size := utf8.DecodeRuneInString(char)
		if !found || char == "" || size != len(char) || r < utf8.RuneSelf || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
		if code == "" || len(code) > 2 || !pluginLowercase(code) || seen[char] {
			return false
		}
		seen[char] = true
		if len(seen) > maxHelpcodeEntries {
			return false
		}
	}
	return len(seen) > 0
}

// validWordbookEntries 是单词本的严格语法：跳过空行和 `#` 开头的行，其余每行是 `word\tmeaning` 或 `word\tphonetic\tmeaning`，不支持引号，列不做裁剪。每条必须满足 WordbookEntry::is_valid（单词 1 到 64 个字符、音标至多 64 个字符可为空、释义 1 到 256 个字符，都不含控制字符）；1 到 20000 条，单词不能重复；任何一行不合法都拒绝整个包。
func validWordbookEntries(raw []byte) bool {
	text, ok := pluginDataText(raw)
	if !ok {
		return false
	}
	bounded := func(s string, max int) bool {
		count := 0
		for _, r := range s {
			if unicode.IsControl(r) {
				return false
			}
			count++
			if count > max {
				return false
			}
		}
		return true
	}
	seen := make(map[string]bool, pluginDataLineCapacity(text, maxWordbookEntries))
	for line := range pluginDataLines(text) {
		word, rest, found := strings.Cut(line, "\t")
		if !found {
			return false
		}
		var phonetic string
		meaning := rest
		if before, after, found := strings.Cut(rest, "\t"); found {
			phonetic, meaning = before, after
			if strings.ContainsRune(meaning, '\t') {
				return false
			}
		}
		if word == "" || !bounded(word, maxWordbookWordChars) || !bounded(phonetic, maxWordbookPhoneticChars) || meaning == "" || !bounded(meaning, maxWordbookMeaningChars) || seen[word] {
			return false
		}
		seen[word] = true
		if len(seen) > maxWordbookEntries {
			return false
		}
	}
	return len(seen) > 0
}

// validPluginSymbols 对应 symbol_set.rs：1 到 32 个 `[[groups]]`，每组 `tab`（"symbols" 或 "kaomoji"）、`title`（必填，1 到 48 字节，与 required_string 相同：非空白、无控制字符）、可选的 `keywords`（与 optional_string 相同：出现时非空白、至多 256 字节、无控制字符）和 `items`（只能是字符串，每组 1 到 512 个，每个 1 到 64 个 UTF-16 单元、非空白、无控制字符，组内不重复）；同一 `tab` 下的组 `title` 不得重复，不同 `tab` 可以同名；全部组合计至多 2048 项。符号集没有数据文件。
func validPluginSymbols(table map[string]any, m pluginManifest) bool {
	rows, _ := table["groups"].([]any)
	if m.Groups == nil || len(*m.Groups) < 1 || len(*m.Groups) > maxSymbolGroups || len(rows) != len(*m.Groups) {
		return false
	}
	const linearLimit = 32
	// 不足 64 项的小组及前 32 项沿用线性查重；大组才创建哈希表，并在各组间清空复用。
	var seen map[string]struct{}
	total := 0
	titles := make(map[[2]string]bool, len(*m.Groups))
	for i, group := range *m.Groups {
		raw, _ := rows[i].(map[string]any)
		if !pluginKeys(raw, "tab", "title", "keywords", "items") || group.Tab == nil || group.Title == nil || group.Items == nil {
			return false
		}
		if *group.Tab != "symbols" && *group.Tab != "kaomoji" || !pluginBoundedText(*group.Title, maxSymbolTitleBytes) {
			return false
		}
		// 客户端按 (tab, title) 定位分组，同一 tab 下标题重复会让两组无法区分。
		key := [2]string{*group.Tab, *group.Title}
		if titles[key] {
			return false
		}
		titles[key] = true
		if group.Keywords != nil && !pluginBoundedText(*group.Keywords, maxSymbolKeywordBytes) {
			return false
		}
		items := *group.Items
		if len(items) < 1 || len(items) > maxSymbolGroupItems {
			return false
		}
		for j, item := range items {
			if !pluginShortText(item, maxSymbolItemUnits) {
				return false
			}
			if len(items) < 2*linearLimit || j < linearLimit {
				if slices.Contains(items[:j], item) {
					return false
				}
				continue
			}
			if j == linearLimit {
				if seen == nil {
					seen = make(map[string]struct{}, len(items))
				} else {
					clear(seen)
				}
				for _, previous := range items[:j] {
					seen[previous] = struct{}{}
				}
			}
			if _, duplicate := seen[item]; duplicate {
				return false
			}
			seen[item] = struct{}{}
		}
		if total += len(items); total > maxSymbolItems {
			return false
		}
	}
	return true
}

// validEffectColor is the client's is_color: "#" and exactly six hexadecimal digits, either case.
func validEffectColor(c string) bool {
	if len(c) != 7 || c[0] != '#' {
		return false
	}
	for i := 1; i < 7; i++ {
		if !strings.ContainsRune("0123456789abcdefABCDEF", rune(c[i])) {
			return false
		}
	}
	return true
}

// The two instants the client expands a template at: both a Wednesday at 23:59:59 with a two-digit day, one in September for the longest English names and one in December for the two-digit month an unpadded %-m gives, so a template that fits at both fits on every day.
var commandTemplateInstants = []time.Time{time.Date(2026, time.September, 30, 23, 59, 59, 0, time.UTC), time.Date(2026, time.December, 30, 23, 59, 59, 0, time.UTC)}

// validCommandTemplate is the rest of the client's validate: the template expands at both instants, and no expansion contains a control character (%n and %t produce them) or exceeds 199 UTF-16 units.
func validCommandTemplate(template string) bool {
	for _, instant := range commandTemplateInstants {
		expanded, ok := expandCommandTemplate(template, instant)
		if !ok || strings.ContainsFunc(expanded, unicode.IsControl) || textutil.UTF16Length(expanded) > maxCommandTemplate {
			return false
		}
	}
	return true
}

// expandCommandTemplate is the client's expand_at: no stray or nested braces, only known placeholders, and every FMT one the client's strftime parser accepts and can format for a date-time without an offset.
func expandCommandTemplate(template string, t time.Time) (string, bool) {
	var out strings.Builder
	rest := template
	for {
		open := strings.IndexAny(rest, "{}")
		if open < 0 {
			out.WriteString(rest)
			return out.String(), true
		}
		if rest[open] == '}' {
			return "", false
		}
		out.WriteString(rest[:open])
		after := rest[open+1:]
		end := strings.IndexAny(after, "{}")
		if end < 0 || after[end] == '{' {
			return "", false
		}
		placeholder := after[:end]
		rest = after[end+1:]
		if placeholder == "weekday" {
			out.WriteString("星期三")
			continue
		}
		name, format, _ := strings.Cut(placeholder, ":")
		switch {
		case name == "date" && format == "":
			format = "%Y-%m-%d"
		case name == "time" && format == "":
			format = "%H:%M"
		case name != "date" && name != "time":
			return "", false
		}
		expanded, ok := pluginStrftime(format, t)
		if !ok {
			return "", false
		}
		out.WriteString(expanded)
	}
}

// pluginStrftime formats t with a strftime description the way the client does it: parse_strftime_borrowed from the time crate, then formatting a PrimitiveDateTime. ok is false for a trailing %, a specifier that parser does not know (%E, %O, %Z and the like), and %s or %z, which parse but need an offset a PrimitiveDateTime does not carry. An optional _, - or 0 after % selects space, no or zero padding for the numeric components that take it; the others ignore it.
func pluginStrftime(format string, t time.Time) (string, bool) {
	year, week := t.ISOWeek()
	hour12 := t.Hour() % 12
	if hour12 == 0 {
		hour12 = 12
	}
	period := "AM"
	if t.Hour() >= 12 {
		period = "PM"
	}
	weekday := int(t.Weekday())
	// 固定指令均为 ASCII，结果非空且忽略填充方式；栈上数组避免重复格式化和 map 扩容。
	var fixed [128]string
	var out strings.Builder
	for i := 0; i < len(format); {
		if format[i] != '%' {
			next := strings.IndexByte(format[i:], '%')
			if next < 0 {
				next = len(format) - i
			}
			out.WriteString(format[i : i+next])
			i += next
			continue
		}
		if i+1 >= len(format) {
			return "", false
		}
		var pad byte
		if c := format[i+1]; c == '_' || c == '-' || c == '0' {
			pad = c
			i++
			if i+1 >= len(format) {
				return "", false
			}
		}
		component := format[i+1]
		i += 2
		if int(component) < len(fixed) && fixed[component] != "" {
			out.WriteString(fixed[component])
			continue
		}
		number := func(value, width int, fallback byte) string {
			p := pad
			if p == 0 {
				p = fallback
			}
			digits := strconv.Itoa(value)
			switch {
			case p == '-' || len(digits) >= width:
				return digits
			case p == '_':
				return strings.Repeat(" ", width-len(digits)) + digits
			default:
				return strings.Repeat("0", width-len(digits)) + digits
			}
		}
		var text string
		cache := true
		switch component {
		case '%':
			text = "%"
		case 'a':
			text = t.Weekday().String()[:3]
		case 'A':
			text = t.Weekday().String()
		case 'b', 'h':
			text = t.Month().String()[:3]
		case 'B':
			text = t.Month().String()
		case 'c':
			text = t.Format("Mon Jan _2 15:04:05 2006")
		case 'D', 'x':
			text = t.Format("01/02/06")
		case 'F':
			text = t.Format("2006-01-02")
		case 'G':
			text = fmt.Sprintf("%04d", year)
		case 'n':
			text = "\n"
		case 'p':
			text = period
		case 'P':
			text = strings.ToLower(period)
		case 'r':
			text = fmt.Sprintf("%02d:%02d:%02d %s", hour12, t.Minute(), t.Second(), period)
		case 'R':
			text = t.Format("15:04")
		case 't':
			text = "\t"
		case 'T', 'X':
			text = t.Format("15:04:05")
		case 'u':
			text = strconv.Itoa((weekday+6)%7 + 1)
		case 'w':
			text = strconv.Itoa(weekday + 1)
		case 'Y':
			text = fmt.Sprintf("%04d", t.Year())
		default:
			cache = false
			switch component {
			case 'C':
				text = number(t.Year()/100, 2, '0')
			case 'd':
				text = number(t.Day(), 2, '0')
			case 'e':
				text = number(t.Day(), 2, '_')
			case 'g':
				text = number(year%100, 2, '0')
			case 'H':
				text = number(t.Hour(), 2, '0')
			case 'I':
				text = number(hour12, 2, '0')
			case 'j':
				text = number(t.YearDay(), 3, '0')
			case 'k':
				text = number(t.Hour(), 2, '_')
			case 'l':
				text = number(hour12, 2, '_')
			case 'm':
				text = number(int(t.Month()), 2, '0')
			case 'M':
				text = number(t.Minute(), 2, '0')
			case 'S':
				text = number(t.Second(), 2, '0')
			case 'U':
				text = number((t.YearDay()-weekday+6)/7, 2, '0')
			case 'V':
				text = number(week, 2, '0')
			case 'W':
				text = number((t.YearDay()-(weekday+6)%7+6)/7, 2, '0')
			case 'y':
				text = number(t.Year()%100, 2, '0')
			default:
				return "", false
			}
		}
		if cache {
			fixed[component] = text
		}
		out.WriteString(text)
	}
	return out.String(), true
}
