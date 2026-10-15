# MSIME Admin Web

水杉管理后台的前端。技术栈：React 19、TypeScript、Vite 8、Tailwind CSS v4、TanStack Router / Query / Table、Radix UI、Recharts、Zod、react-hook-form、cmdk、date-fns、lucide-react、pnpm 和 Biome。不使用 Sass，样式全部是 Tailwind 工具类加 `src/styles/tokens.css` 中的设计变量。

```sh
pnpm install --frozen-lockfile
pnpm check   # tsc --noEmit
pnpm lint    # biome lint
pnpm test    # 词库表格模型和公共工具测试（Node 24）
pnpm build   # tsc && vite build，输出 dist/
pnpm dev
```

## 目录

- `src/main.tsx`：入口。第一行导入 `src/zod-config.ts`，让 Zod 以 jitless 模式运行；`vite.config.ts` 把 zod 和这个文件放进同一个 chunk，保证任何共享 chunk 建 schema 之前 jitless 已生效。
- `src/routes/router.tsx`：路由表。每个页面是 `src/pages/<page>/index.tsx` 的默认导出，懒加载为独立 chunk；旧路径（`/admins`、`/audit`、`/system`、`/crashes`、`/skins`、`/dictionaries`、`/replies`）在这里重定向。新增页面时同步修改 `src/nav.ts` 和 Go 侧 `embed.go` 的 `pagePaths`。
- `src/api/`：每个页面一个模块，放 Zod schema、类型和标签。`client.ts` 提供 `useAPI()`（绑定当前登录身份，401 时自动登出）、`requestAPI()`，以及所有服务端错误码的中文提示 `codeMessages`；新增错误码在这里加文案，页面只在语境需要不同措辞时局部覆盖。`keys.ts` 是 TanStack Query 的 key 工厂。
- `src/utils/`：不依赖组件的公共工具。`time.ts` 提供本地时区日期格式化与中文相对时间，无效日期保留原文；`bytes.ts` 统一字节到 B、KB、MB 的单位换算，页面自行决定精度和空值文案。
- `src/hooks/`：共享 React Hook。`use-debounced.ts` 延迟发布输入值，输入或延迟改变时重新计时，卸载时取消等待。
- `src/shell/`：外壳（侧栏、顶栏、全局搜索、通知、外观、权限钩子 `usePermissions`、页面级快捷键）。
- `src/ui/`：共享组件。`data-table.tsx`（选择、批量操作、本页筛选、分页）、`drawer.tsx`、`confirm.tsx`、`toast.tsx`（4 秒撤销与延迟提交）、`filter-chips.tsx`、`segmented.tsx`、`stat-tile.tsx`、`charts.tsx`、`states.tsx` 等。`overlay.ts` 记录打开的弹窗：页面快捷键在弹窗打开时失效，提示条把 Escape 交给最上层弹窗，弹窗不会因点击提示条的「撤销」而关闭。
- `src/auth.tsx`：登录状态、Google 登录与管理员密钥登录页。
- `tests/csp_smoke.py`、`tests/harness/`：CSP 冒烟测试与共享组件测试页。

## CSP

后台的 CSP 是 `script-src 'self'; style-src 'self'; img-src 'self' data:`，不允许 eval、内联 `<style>` 和外链图片。因此：

- 不使用 Radix 的 `Dialog.Overlay`（其滚动锁会注入 `<style>`），弹窗用普通元素做遮罩。
- 动态样式只能写成 Tailwind 类或 React 的 `style` 属性（CSSOM 设置，不受 `style-src` 限制）。
- 改动依赖、共享组件或构建配置后，运行 CSP 冒烟测试（需要 Python 版 Playwright 与 Chromium）：

```sh
pnpm build
python3 tests/csp_smoke.py
```

测试在模拟后端上逐页打开 `dist/`，检查 CSP 违规、旧路径重定向、外壳弹层，以及 harness 中的表格、确认框、提示条、抽屉和图表交互。认证回归测试还会控制请求完成顺序，确认 Cookie 登录、同一密钥重新登录后，旧请求的 `401` 和旧退出操作不会清空新身份或覆盖新身份的错误提示；退出等待延迟提交时若身份已改变，也不会再发送旧退出请求。

个人中心回归测试会挂起通知偏好保存及后续刷新，确认全部偏好开关在等待期间不可操作，切走再返回仍不能重复提交；成功后允许继续切换，失败后恢复原值并可重试。另在旧身份保存请求完成前切换登录，再分别返回 `401` 和 `500`，确认失败回滚不会恢复旧身份资料；当前身份保存失败时仍会恢复原来的偏好值。

公告回归测试会挂起保存、发布和归档请求，确认等待期间不能修改内容、取消编辑、切换草稿或发起另一项写入；成功后重置表单，失败后恢复编辑并保留原文。全局搜索跳转到另一条草稿时，等当前请求完成后再打开，避免新草稿被旧请求的完成处理清空。

下载镜像回归测试会挂起链接保存和清空请求，确认输入框和写入按钮在等待期间不可操作，成功后显示服务端确认的链接，失败后恢复编辑并保留草稿。

敏感词、管理员添加和 Issue 回复回归测试会在请求等待期间继续输入，确认成功回调只清理已提交且未继续编辑的草稿，失败时保留原文。Issue 回复还会关闭并重新打开同一条或另一条 Issue，确认即使新旧回复正文相同，旧请求也不会清空新草稿。

发布管理回归测试会挂起触发发布和保存说明请求，确认版本号与发布说明在等待期间不可编辑，弹窗不能取消或按 Escape 关闭；成功后关闭弹窗，失败后保留原文并允许修改后重试。

词库 PR 修剪回归测试会挂起“仅保留勾选项”请求，确认等待期间不能改选或使用按钮、快捷键再次写入；切到其他 PR 再返回后仍保留等待保护，其他 PR 可继续审核。成功后保留主动收录的重复词条选择，失败后保留原选择并恢复编辑。

## 构建产物

`src/` 为前端源码，`dist/` 为提交到版本库的生成产物，每次前端变更都需重新 build 并一起提交。Go 的 `embed.go` 嵌入 `dist/`；Docker 的 Node 构建阶段会自动重建，然后编入 Go 二进制，运行容器不需要 Node。

开发代理默认连接 `127.0.0.1:18089`，Go 本地配置需设 `admin.host: admin.localhost`。登录方式（Google OIDC、管理员密钥、个人访问令牌）、角色权限、配置项和接口说明见 [管理后台文档](../docs/admin.md)。
