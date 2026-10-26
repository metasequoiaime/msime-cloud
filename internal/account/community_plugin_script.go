package account

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/textutil"
)

// 脚本插件（`kind = "script"`）的服务端规则，对应客户端 plugins/script_pack.rs 与 msime_script_runtime 的能力表。
//
// 服务端只做不执行代码就能做的检查：清单的键和取值、能力名与组合规则、模块与数据文件存在且大小在上限内、模块以 wasm 文件头开头。模块本身（导入表、导出、与申请的能力是否一一对应）只有客户端的 wasm 校验器能判断，客户端在发布前（`pack` 里的 `validate`）和每次安装（`install` 里的 `validate`）都用同一份校验器重新检查，所以这里的规则只会比客户端宽，不会比它严：服务端拒绝一个客户端能装的包，用户就没有办法发布它。
//
// 能力名和清单键包括第二批能力的提案（`.agents/notes/proposed/architecture/2026-10-26-script-plugin-capabilities-v2.md`）：`insert_candidates`、`persist_data`、`[[script.settings]]`、`[script].data` 和 `[[script.commands]].arguments`，客户端实现它们时不必再等一次服务端发版。
const (
	// pluginScriptSchemaVersion 是脚本插件清单的 schema_version；其余类型仍只认 1。
	pluginScriptSchemaVersion = 2
	pluginScriptABI           = 1
	maxScriptModuleBytes      = 256 << 10
	maxScriptMemoryPages      = 64
	maxScriptCommands         = 4
	minScriptTriggerBytes     = 2
	maxScriptTriggerBytes     = 16
	maxScriptTitleChars       = 8
	maxScriptDataFiles        = 4
	maxScriptDataFileBytes    = 4 << 20
	maxScriptDataBytes        = 8 << 20
	maxScriptSettings         = 16
	maxScriptSettingKeyBytes  = 32
	maxScriptSettingTitle     = 24
	maxScriptSettingDesc      = 120
	minScriptChoiceOptions    = 2
	maxScriptChoiceOptions    = 16
	maxScriptTextSetting      = 256
)

// scriptCapabilities 是客户端 msime_script_runtime::Capability 的能力名，含第二批的 insert_candidates 和 persist_data。
var scriptCapabilities = []string{"read_composition", "read_commit", "read_candidates", "rewrite_commit", "rerank_candidates", "annotate_candidates", "filter_candidates", "provide_commands", "insert_candidates", "persist_data"}

// scriptCapabilityRequires 是 Capability::requires：申请左边的能力必须同时申请右边的。
var scriptCapabilityRequires = map[string]string{
	"rewrite_commit":      "read_commit",
	"rerank_candidates":   "read_candidates",
	"annotate_candidates": "read_candidates",
	"filter_candidates":   "read_candidates",
	"insert_candidates":   "read_candidates",
}

// scriptCapabilityExclusive 是 CapabilitySet::check_rules 的互斥对：改写上屏不能和读预编辑串或持久存储同时申请。
var scriptCapabilityExclusive = [][2]string{{"rewrite_commit", "read_composition"}, {"rewrite_commit", "persist_data"}}

// scriptSensitiveCapabilities 是审核列表要标红的能力：能改掉用户上屏的文字，或能把读到的文字长期留在本机。
var scriptSensitiveCapabilities = []string{"rewrite_commit", "persist_data"}

// scriptHooks 是 msime_script_runtime::Hook 的钩子名。
var scriptHooks = []string{"focus_in", "focus_out", "input", "selection_retracted", "candidate_selected", "before_commit", "commit", "composition_end", "composition_start", "composition_update", "candidates_shown", "candidates_hidden", "candidates", "command"}

// scriptBuiltinTriggers 和 scriptBuiltinTitles 是客户端 script_pack.rs 的 BUILTIN_COMMAND_TRIGGERS 与 BUILTIN_COMMAND_TITLES。
var (
	scriptBuiltinTriggers = []string{"rq", "riqi", "date", "sj", "shijian", "time", "xq", "xingqi", "week", "nl", "nongli", "yinli", "translate", "fanyi", "fy"}
	scriptBuiltinTitles   = []string{"日期", "时间", "星期", "农历", "翻译"}
)

// pluginScriptManifest 是 `[script]` 表的类型视图。Default 的类型取决于设置项的 type，所以读成 any 再按 type 检查。
type pluginScriptManifest struct {
	ABI         *int64    `toml:"abi"`
	Module      *string   `toml:"module"`
	Hooks       *[]any    `toml:"hooks"`
	MemoryPages *int64    `toml:"memory_pages"`
	Data        *[]string `toml:"data"`
	Commands    *[]struct {
		Trigger   *string `toml:"trigger"`
		Title     *string `toml:"title"`
		Arguments *string `toml:"arguments"`
	} `toml:"commands"`
	Settings *[]struct {
		Key         *string `toml:"key"`
		Title       *string `toml:"title"`
		Description *string `toml:"description"`
		Type        *string `toml:"type"`
		Default     any     `toml:"default"`
		Options     *[]struct {
			Value *string `toml:"value"`
			Title *string `toml:"title"`
		} `toml:"options"`
		Min       *int64 `toml:"min"`
		Max       *int64 `toml:"max"`
		MaxLength *int64 `toml:"max_length"`
	} `toml:"settings"`
}

// validScriptPermissions 对应客户端 parse_permissions 与 check_rules：每一项是白名单里的能力名，不重复，依赖一起申请，互斥的不同时申请。不写等于不申请。返回按清单顺序的能力名。
func validScriptPermissions(items *[]any) ([]string, bool) {
	if items == nil {
		return []string{}, true
	}
	permissions := make([]string, 0, len(*items))
	for _, item := range *items {
		name, ok := item.(string)
		if !ok || !slices.Contains(scriptCapabilities, name) || slices.Contains(permissions, name) {
			return nil, false
		}
		permissions = append(permissions, name)
	}
	for _, name := range permissions {
		if required, ok := scriptCapabilityRequires[name]; ok && !slices.Contains(permissions, required) {
			return nil, false
		}
	}
	for _, pair := range scriptCapabilityExclusive {
		if slices.Contains(permissions, pair[0]) && slices.Contains(permissions, pair[1]) {
			return nil, false
		}
	}
	return permissions, true
}

// scriptTextOK 是标题、说明这类插件文本的服务端检查：UTF-8、不含控制字符、首尾不是空白、字符数在 1 到 max 之间。客户端另外拒绝一批格式和不可见字符（abi::has_forbidden_plugin_char），那份清单由安装时的校验负责，这里不复制，免得比客户端严。
func scriptTextOK(s string, min, max int) bool {
	n := utf8.RuneCountInString(s)
	return utf8.ValidString(s) && n >= min && n <= max && !strings.ContainsFunc(s, unicode.IsControl) && strings.TrimSpace(s) == s
}

// scriptTriggersOverlap 对应客户端 triggers_overlap：相等或一个是另一个的前缀。
func scriptTriggersOverlap(left, right string) bool {
	return strings.HasPrefix(left, right) || strings.HasPrefix(right, left)
}

// validPluginScript 对应 script_pack.rs 的 parse 与第二批提案的清单键，返回清单点名的模块和数据文件（文件名到上限），以及数据文件的名字。permissions 是已经通过 validScriptPermissions 的能力名。
func validPluginScript(table map[string]any, m pluginManifest, permissions []string) (map[string]int, []string, bool) {
	raw, _ := table["script"].(map[string]any)
	s := m.Script
	if s == nil || !pluginKeys(raw, "abi", "module", "hooks", "memory_pages", "commands", "data", "settings") {
		return nil, nil, false
	}
	if s.ABI == nil || *s.ABI != pluginScriptABI {
		return nil, nil, false
	}
	if s.Module == nil || !validPluginFileName(*s.Module) || pluginExtension(*s.Module) != "wasm" {
		return nil, nil, false
	}
	module := *s.Module
	if s.Hooks == nil || len(*s.Hooks) == 0 {
		return nil, nil, false
	}
	hooks := make([]string, 0, len(*s.Hooks))
	for _, item := range *s.Hooks {
		hook, ok := item.(string)
		if !ok || !slices.Contains(scriptHooks, hook) || slices.Contains(hooks, hook) {
			return nil, nil, false
		}
		hooks = append(hooks, hook)
	}
	if s.MemoryPages != nil && (*s.MemoryPages < 1 || *s.MemoryPages > maxScriptMemoryPages) {
		return nil, nil, false
	}
	hasCommands := s.Commands != nil
	if hasCommands && !validScriptCommands(raw, s) {
		return nil, nil, false
	}
	// 指令、provide_commands 和 command 钩子三者同在或同不在。
	if hasCommands != slices.Contains(permissions, "provide_commands") || hasCommands != slices.Contains(hooks, "command") {
		return nil, nil, false
	}
	if s.Settings != nil && !validScriptSettings(raw, s) {
		return nil, nil, false
	}
	files := map[string]int{module: maxScriptModuleBytes}
	var data []string
	if s.Data != nil {
		if len(*s.Data) > maxScriptDataFiles {
			return nil, nil, false
		}
		for _, name := range *s.Data {
			if !validPluginFileName(name) || name == "plugin.toml" || name == module || slices.Contains(data, name) {
				return nil, nil, false
			}
			data = append(data, name)
			files[name] = maxScriptDataFileBytes
		}
	}
	return files, data, true
}

// validScriptCommands 对应 parse_commands：1 到 4 条，触发词 2 到 16 个小写字母，不与内置触发词、也不与同清单的触发词相等或互为前缀；标题 1 到 8 个字符，不等于内置标题；arguments 可选，只能是 "letters" 或 "extended"。
func validScriptCommands(raw map[string]any, s *pluginScriptManifest) bool {
	rows, _ := raw["commands"].([]any)
	commands := *s.Commands
	if len(commands) < 1 || len(commands) > maxScriptCommands || len(rows) != len(commands) {
		return false
	}
	triggers := make([]string, 0, len(commands))
	for i, row := range commands {
		keys, _ := rows[i].(map[string]any)
		if !pluginKeys(keys, "trigger", "title", "arguments") || row.Trigger == nil || row.Title == nil {
			return false
		}
		trigger, title := *row.Trigger, *row.Title
		if len(trigger) < minScriptTriggerBytes || len(trigger) > maxScriptTriggerBytes || !pluginLowercase(trigger) {
			return false
		}
		for _, other := range append(slices.Clone(scriptBuiltinTriggers), triggers...) {
			if scriptTriggersOverlap(trigger, other) {
				return false
			}
		}
		triggers = append(triggers, trigger)
		if !scriptTextOK(title, 1, maxScriptTitleChars) || slices.Contains(scriptBuiltinTitles, title) {
			return false
		}
		if row.Arguments != nil && *row.Arguments != "letters" && *row.Arguments != "extended" {
			return false
		}
	}
	return true
}

// validScriptSettings 对应第二批提案的 `[[script.settings]]`：至多 16 项；key 是 1 到 32 个小写字母、数字或 `_`，不重复；title 1 到 24 个字符；description 至多 120 个字符；type 是 bool、choice、number 或 text，默认值和附加键按类型检查。
func validScriptSettings(raw map[string]any, s *pluginScriptManifest) bool {
	rows, _ := raw["settings"].([]any)
	settings := *s.Settings
	if len(settings) > maxScriptSettings || len(rows) != len(settings) {
		return false
	}
	keys := make([]string, 0, len(settings))
	for i, setting := range settings {
		fields, _ := rows[i].(map[string]any)
		if !pluginKeys(fields, "key", "title", "description", "type", "default", "options", "min", "max", "max_length") {
			return false
		}
		if setting.Key == nil || setting.Title == nil || setting.Type == nil {
			return false
		}
		key := *setting.Key
		if key == "" || len(key) > maxScriptSettingKeyBytes || strings.ContainsFunc(key, func(c rune) bool { return !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') }) || slices.Contains(keys, key) {
			return false
		}
		keys = append(keys, key)
		if !scriptTextOK(*setting.Title, 1, maxScriptSettingTitle) || setting.Description != nil && !scriptTextOK(*setting.Description, 0, maxScriptSettingDesc) {
			return false
		}
		switch *setting.Type {
		case "bool":
			if _, ok := setting.Default.(bool); !ok {
				return false
			}
		case "choice":
			if setting.Options == nil || len(*setting.Options) < minScriptChoiceOptions || len(*setting.Options) > maxScriptChoiceOptions {
				return false
			}
			options, _ := fields["options"].([]any)
			if len(options) != len(*setting.Options) {
				return false
			}
			values := make([]string, 0, len(*setting.Options))
			for j, option := range *setting.Options {
				optionKeys, _ := options[j].(map[string]any)
				if !pluginKeys(optionKeys, "value", "title") || option.Value == nil || option.Title == nil || *option.Value == "" || slices.Contains(values, *option.Value) || !scriptTextOK(*option.Title, 1, maxScriptSettingTitle) {
					return false
				}
				values = append(values, *option.Value)
			}
			if value, ok := setting.Default.(string); !ok || !slices.Contains(values, value) {
				return false
			}
		case "number":
			value, ok := setting.Default.(int64)
			if !ok || setting.Min == nil || setting.Max == nil || !scriptInt32(*setting.Min) || !scriptInt32(*setting.Max) || *setting.Min > value || value > *setting.Max {
				return false
			}
		case "text":
			value, ok := setting.Default.(string)
			if !ok || setting.MaxLength == nil || *setting.MaxLength < 1 || *setting.MaxLength > maxScriptTextSetting || int64(textutil.UTF16Length(value)) > *setting.MaxLength || !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func scriptInt32(v int64) bool { return v >= -1<<31 && v <= 1<<31-1 }

// validScriptFiles 是 validPluginFiles 之后脚本包自己的文件检查：模块以 wasm 文件头（`\0asm` 和版本 1）开头，数据文件合计不超过 8 MiB。存在、非空和单个文件的上限已由 validPluginFiles 按 files 检查过。
func validScriptFiles(members map[string]pluginMember, module string, data []string) string {
	if head := members[module].head; len(head) < 8 || string(head[:8]) != "\x00asm\x01\x00\x00\x00" {
		return "invalid_plugin_manifest"
	}
	total := 0
	for _, name := range data {
		total += members[name].size
	}
	if total > maxScriptDataBytes {
		return "plugin_too_large"
	}
	return ""
}
