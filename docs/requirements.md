# 共通后端需求与验收记录

核对日期：2026-09-08。组织 GitHub 仓库清单已通过 `gh repo list metasequoiaime --limit 100` 核对。现役输入法产品是 Windows、Apple（macOS/iOS）、Linux；Web 是官网。旧 MSIME-Windows-Server 已归档，其代码在 Windows/server，是 Windows 本地宿主。本目录为新的共通网络服务。

## 源码依据

- Windows README 的云联想、AI 联想、候选翻译及 API 配额问题；`server/src/cloud/custom_translation.cpp` 的 DeepLX 请求和响应格式；`server/src/cloud/tencent_tmt.cpp` 的腾讯 TMT 适配。
- Linux README 的异步云候选、AI、DeepLX 翻译、语音与润色；`src/online/GoogleCloudProvider.cpp` 固定 Google 地址和拼音/日语查询；`src/online/AiSuggestionProvider.cpp` 的 Chat Completions 协议；`src/VoiceInput.h` 的转写及润色端点。
- Engine `voice/README.md`：公共非流式 multipart 转写适配，Windows 专属流式传输仍在 Windows/server。
- Apple README：macOS 语音录音与取消规则；Docs `architecture/platform-adoption.md`：iOS 暂无语音入口。
- `.github/AGENTS.md`：本地输入算法归 Engine，系统适配归平台；异步结果须验证会话代次；不得记录真实输入和凭据。

## 交付验收（持续更新）

| 需求 | 当前证据 | 状态 |
|---|---|---|
| Go 共通服务与配置、启动、优雅关闭 | cmd/msime-server、internal/server；本地编译，真实二进制 health/鉴权 capabilities/SIGTERM 冒烟通过 | 已实现并完成本地启动验收 |
| 统一服务商凭据、客户端认证、限流、并发、超时、大小上限 | server.go/config.go；竞态测试通过 | 实现完成；限流按 `replicas` 在各副本间分摊，并发上限按副本计算 |
| 云候选，拼音与日语 | cloud handler、Google 模拟响应测试 | 后端与 Windows/Linux/macOS/iOS 接入已实现；Windows/Linux TLS 联合测试通过 |
| AI 联想 | chat handler；模型固定及鉴权测试 | 已按 Windows/Linux 实际 JSON 请求补齐 response_format，兼容回归通过；保留各客户端既有提示词与候选格式；Linux 实际 AI 客户端 TLS 验证通过 |
| 候选翻译 | DeepLX handler 与测试 | DeepLX 与腾讯 TMT 适配已实现；TC3 固定向量和合成 HTTPS 上游通过，真实腾讯账户未验收 |
| 语音转写及润色 | multipart handler / chat handler 与测试 | 批量已实现；新增 WSS 流式转发与 Windows 设备令牌接入，合成 TLS 测试通过，真实供应商/宿主未验收 |
| Windows 接入 | 新增云候选配置、设置页双宿主桥接、请求快照、可取消 curl 传输；2 项候选协议测试通过；TypeScript/Vite 构建与 11 项既有页面测试通过 | 已实现云候选适配；Windows 原生联合验证待做，AI/翻译/语音复用现有自定义端点 |
| macOS / iOS 接入 | Foundation 共用云候选客户端、Keychain 配置、Engine 合并、iOS App 设置与完全访问门禁、macOS 菜单设置；iOS 模拟器 SDK 构建通过 | 源码接入已实现；匹配固定 Engine 的 macOS 通用构建与 41/41 测试通过；iOS 真机/真实服务联合验证待做 |
| Linux 接入 | 新增 MSIME 云候选端点、密钥环与设置 UI；AI/翻译/语音已有自定义端点；完整 Linux 编译和 39/39 ctest 通过（含 IBus 冒烟和打包） | 云候选适配已完成；实际云候选适配器与 Go 服务 TLS 联合测试通过；AI/翻译/语音实际服务类 TLS 联合测试通过；完整宿主联网验收待做 |
| 公共 HTTP 契约的权威归属 | 本仓 contracts/protocol.json（原为 MSIME-Engine contracts/backend/protocol.json 的副本，Engine 已归档）；生成常量、HTTP/TLS 契约测试 | 契约已实现并通过一致性测试 |
| 部署、CI、维护文档 | Docker 构建及只读容器 health/鉴权/capabilities/SIGTERM 验证通过；CI 检查契约生成 | 本地容器已验证；远程 CI 和生产部署未执行 |
| 真正平台端到端验收 | Linux/Windows 云候选适配器经 TLS 访问真实 Go 服务通过；Go 自身所有契约操作经过 HTTP/TLS | 候选及 AI/翻译/批量语音服务类网络链路已验证；Windows TSF、Apple 宿主联网仍未完成 |

账号注册、跨设备用户词库同步、配置同步在已检查的现役在线接口中没有现成协议；需要继续核对产品架构与需求，不能凭空宣称已经要求或已经支持。当前不上传用户词库、配置、剪贴板或遥测。全局月度成本额度、分布式配额与开放注册也未实现，现阶段部署为管理员发放令牌的单实例服务。

下一步：完成 Windows 实时语音原生宿主验证；执行 Apple 宿主联网与 Windows 原生验证；检查产品架构中是否存在尚未覆盖的共通在线需求。公共契约、各平台云候选配置与批量服务类 TLS 联合测试已经完成。当前不能标记总体目标完成。

2026-09-08 追加验证：Docker 镜像 `msime-linux-build:full` 内执行 CMake Debug 全量构建和 `ctest --output-on-failure --timeout 30`，39/39 通过。Go `go test -race ./...` 与 `go vet ./...` 通过。该证据包含 Linux 原生构建/IBus 冒烟，但不代表真实后端联合验收或 Windows/Apple 已接入。

Windows 验证限制：候选 C++ 代码在 Linux 构建镜像中以 `-Wall -Wextra -Werror` 编译测试，不能代替 Windows 原生构建。页面标准 `pnpm build` 的 prebuild 被既有 Engine 工作区 gitlink 差异阻止：HEAD 固定 `8eb4b4e`，当前 vendor 为 `6bd2254`；已用 git show 证明 HEAD 的 shared/schema.js 与 HEAD 固定 Engine 字节一致。本任务未更改 gitlink 或生成契约副本；`tsc`、`vite build` 以及既有 11 个 vitest 独立通过。

Apple 验证记录：iOS App/Keyboard 通过 Xcode 26.6 iphonesimulator Debug 编译（未签名）；BackendClient Foundation 测试覆盖主线程交付、认证头、取消和恶意候选；Adapter 测试验证相同拼写的新组合拒绝旧响应。macOS 全量构建遇到既有 vendor 差异：HEAD 固定 b411f68，当前为 020e906，缺少控制器引用的 contracts/punctuation/policy.h；未修改该 gitlink，后续已用下文隔离的匹配依赖完成整体验证。

新增验证：Go race/vet、全部契约示例 HTTP/TLS 测试通过；WAV FuzzWAV 3 秒执行 144016 次无异常。Engine 既有 15 项 ctest 全通过（本轮仅新增 HTTP 清单与文档）。容器 msime-server:verification 构建并完成 smoke_container.py 验证。

匹配依赖验证已完成：在 /tmp/msime-apple-backend-verify 保留本任务 Apple 源码，按 Apple HEAD 固定 b411f68 及其子模块创建隔离源码快照；修复新增 CandidateSource 命名空间错误后，arm64+x86_64 应用构建、签名和 41/41 ctest（含 release_package）通过。原工作区 vendor 未改变。

实际客户端网络测试：`python3 scripts/native_e2e.py` 编译 MSIME-Linux 的 GoogleCloudProvider/CurlHttpTransport 与 MSIME-Windows 的 CloudIme::Fetch，在一次性容器中建立测试 CA，访问 Go HTTPS 服务，Windows/Linux 两个客户端均通过鉴权、输入编码、上游转发和候选解析。Windows 代码在 Linux 编译，此结果不是 Windows TSF 原生宿主验证。

原生服务扩展测试已通过：Linux AI、翻译、语音转写和润色；Windows 自定义翻译及批量语音请求/响应；Apple 使用的 Engine CloudSttWorker/TextPolisher 共享类；错误设备令牌被拒绝。运行入口仍为 `scripts/native_e2e.py`，专用测试镜像由脚本自动构建。

流式核对：Windows `doubao_asr_client.{h,cpp}` 在录音期间发送 16kHz 单声道 PCM，使用 WebSocket 二进制帧、负数末包序号与实时转写回调；鉴权为 X-Api-Key 或 App Key/Access Key，另有 Resource ID。它不能直接连接 multipart 转写接口，现已新增独立 `/v1/audio/stream` 转发、服务端凭据替换和 Windows `msime-stream` 提供商；消息上限、二进制保真、鉴权、到期和关闭测试通过。Windows 流式源文件 MinGW Windows 目标 `-fsyntax-only -Wall -Wextra -Werror` 通过，设置页 tsc/Vite 和 11 项测试通过；尚非 Windows TSF/录音运行验证。

Apple 真实网络补充：`scripts/apple_e2e.py` 已编译实际 MSIMEBackendClient.m，使用 NSURLSession 连接 Go HTTPS 服务，拼音/日语、错误令牌、未受信任证书、在途取消传递到上游、下一代请求和主线程回传全部通过。测试信任锚仅限测试进程，不修改 Keychain 或系统信任。此结果验证共享网络 SDK，仍不代替 iOS 完全访问/共享 Keychain 或 IMK 宿主验收。

在线功能复核：Windows 手写使用系统 InkRecognizerContainer；Linux 手写调用本地 Tesseract。这两个入口没有待迁移的供应商网络请求。现役源码的在线输入能力已覆盖云候选、AI、翻译、批量语音与实时语音；未发现现成账号或跨设备同步协议，本任务不凭空新增用户数据上传。

剩余原生验收的执行步骤与证据要求见 [native-acceptance.md](native-acceptance.md)。2026-09-08 环境核对：当前为 macOS；xctrace 仅列出本机及模拟器，登记的 iPhone 均离线；未发现 prlctl/VBoxManage/virsh 命令。Windows 测试机或 CI runner、已签名 iOS 测试设备的可用性已询问用户，尚未得到确认。
