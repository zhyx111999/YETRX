# 验证记录

## v0.2.5-yetrx.5 / .6：子任务与子代理的定位区分

- 原版 Planner/Worker 体系统一为「子任务（探索）」：围绕探索目标持续规划、领取探索意图并执行。系统设置显示「子任务执行并发（探索）」，明确每个探索任务默认 3 个执行槽位，不是累计任务数；任务列表、执行会话、操作反馈和内置角色说明同步更新。
- 新增 `subagents` 统一为「子代理（对话）」：按需独立求证、审查或小范围修改，返回实际操作、依据与结果，由主对话核验并决定采纳/合并。工具说明、父代理/子代理引导、后台返回、文件发送说明、README 和三份技能统一定位；全局并发仍为 4，不增加新的并发配置。
- 保留 `workers`、`spawn_task`、`subagents`、`subagents_control`、tasks/depends_on、各类 ID、会话与调度逻辑。定位仅在请求 system 追加，不改写用户提示词正文、原始消息、活动或历史 transcript。
- 集中迁移精确已发布的默认工具文案，只有 control label 的精确旧参数说明会更新；自定义描述、参数默认值、额外 schema、绑定、开关不变，CAS 防止覆盖并发编辑。内置角色播种也只替换精确旧默认名称/描述，保留用户编辑与预算。文件发送旧默认说明迁移合并到同一模块，不叠加重复升级逻辑。
- Windows 全仓库 Go 编译、受影响 agent/db/server 的 vet、workflow 全包测试和 Agent 定位/steering/子工具约束/thread/Persona 聚焦测试通过。前端 TypeScript 和 32 页生产导出通过，`git diff --check` 通过；本轮未重跑 Linux race 或真实 Astra，不将文案定位测试当成模型能力验收。
- 独立 PostgreSQL 库 `yetrx_terms_20261014_test` 的实际测试全部通过、无 skip：角色旧默认升级/重启保留用户编辑、五种工具默认文案升级/幂等/自定义参数保留、原 schema 升级、文件工具说明保留，以及原生子代理共享资源/延迟回传/取消回归。最初宽匹配包含 opt-in 实模型用例而自然 skip，随后排除它并完整重跑上述确定范围，不计实模型测试为通过。
- Edge 桌面 1440px、移动 390px 使用生产构建及受控 API 验证两套定位说明、保存探索并发的请求仍只有 `workers` 字段、任务页说明和无横向溢出/运行错误。前端全文件 Biome 仍有既存格式/规则问题，不声称全量 lint 通过。
- 发布前只读核对线上技能：workflow-authoring 和 subagents-setup 与历史提交 `671e406c9f2b` 的原始默认全文一致，workflow-patterns 与 `ab168c54dfd2` 默认一致；不是凭旧版本 hash 不同就认定用户编辑。部署仅在当次文件仍与已核对全文一致时更新，否则保留并报告。

`.5` 源码提交 `943275388d1e744c957a41d481f418b827b4fb8a`、Actions `38547093151` 前端/五平台/release 全部成功，Docker 按配置跳过。ZIP SHA256 `45bdd6a00608e17074bacd8ddcf6de5260ae52c3b7ab13414586e40c203371db`，二进制 SHA256 `eb663032a99dc996dddc7ba3c65c11df5fa972b6b5e0c45eec65cd30f34c24b7`。

- 第一次严格文件校验因流量索引 SQLite 的 `index.sqlite-wal`/`index.sqlite-shm` 在启动时变化而自动回退 `.4`；其他数据库/文件检查通过。第二次为 SQLite 增加独立一致性备份、integrity_check 和逻辑行摘要校验，仅不按字节比较这两个运行辅助文件；索引主文件及其余文件仍按 hash 核对。切换后与当次快照以及首次原始备份复核通过，没有恢复或覆盖生产数据库。
- `.5` 上线后的当次核对：48 张受保护表（含 settings，Agent 名称/说明只允许精确旧默认升级；tools 另作逐字段比较）、5986 个文件；19 个会话、3321 条活动、1 个模型配置、10 条提示词版本、1 个人格保留。JWT、systemd、其他 settings 和用户工具偏好均保持，三份已核实为历史默认的技能已升级。回滚根目录 `/opt/artex/_rollback-v0.2.5-yetrx.5`，第二次证据位于 `attempt-2`。
- 随后只读 API 核验发现线上 subagents 仍保留最早 `671e406c9f2b` 版本的默认说明，早于 `.5` 首批兼容列表；不是用户自定义。`.6` 补齐该完整旧默认及 control label 的「read 指定一个子任务」旧说明。已遍历 main 中该文件所有历史文案版本核对，独立 PostgreSQL 重新通过所有上述测试，覆盖最早与后续两类 label 文案，仍不覆盖自定义内容。

`.6` 的发布与最终线上核验结果将在完成后补充。

## v0.2.5-yetrx.4：可配置的 Agent 文件大小上限

- 系统配置新增独立「Agent 文件发送」卡片。`chat_file_max_mib` 使用既有 settings 存储，默认 128 MiB，0 表示不限制；保存后由后续每次发送动态读取，无需重建 Agent 或重启。前后端校验非负整数及安全字节换算范围。
- 文件复制的初始大小与实际复制字节都使用本次读取的上限。修改上限不会影响正在复制的文件，也不影响已发附件下载；用户上传接口继续使用原来的请求体限制。
- 下载改为认证 HEAD 预检，再通过同源认证 cookie 交给浏览器直接下载；不再将整个文件读成 JavaScript Blob，不在 URL 里传 token。
- 升级时仅替换 `send_file` 原有默认说明里的固定上限文字；用户说明、参数默认值、绑定和开关保持。
- 独立 PostgreSQL 数据库实际执行 `TestChatFileSettingsRuntimeLimitAndPersistence`、`TestChatFileDescriptionUpgradePreservesCustomTool`、原附件复制/认证下载测试，以及受影响的 finding/notify 设置回归测试，全部通过，无 skip。覆盖数据库持久化、新服务实例读取、同一工具实例连续改限、恰好上限与超一字节、实际发送 129 MiB + 1 字节、0 不限大小、旧附件降限后下载、部分更新、非法配置修复，以及 HEAD/Bearer 与 GET/cookie 下载。
- Linux 服务端测试二进制编译、`go vet ./server`、前端 TypeScript 和 32 页生产静态导出、新配置组件 Biome 检查通过。Windows 本机无数据库时的两项数据库测试未在本机完成，已由上述独立 PostgreSQL 实跑覆盖。
- Edge 桌面 1440px、移动 390px 使用生产构建和受控设置 API 验证保存 1024、刷新回显、保存 0、拒绝负数/小数/空输入、服务端失败保留原值。附件下载走实际本地 HTTP 响应，验证认证 HEAD、cookie GET、中文名称与完整字节，页面运行错误为 0。

源码提交 `a089b0cc806fa4f96e22c5b40d4d38fd501e7a9e` 已推送并发布 [v0.2.5-yetrx.4](https://github.com/zhyx111999/YETRX/releases/tag/v0.2.5-yetrx.4)。Actions `38540175058` 前端、五平台构建和 release 全部成功，Docker 按配置跳过。

- Linux amd64 ZIP SHA256 `eed611a19f6ab86624c62c42574fabbbb0d03d64388e84de24873645800914f9` 已按 SHA256SUMS 校验；线上二进制 SHA256 `f4ef44acffe9ee493f526f30fa0c6bb8343e9c26717ccbe5d8cb6cf0949c6311` 与发布资产一致。
- 生产健康版本 `0.2.5-yetrx.4`，systemd active；认证 settings API 返回 `chat_file_max_mib:128`，未认证修改被拒绝。没有在生产修改默认上限进行测试。
- 线上真实 API 与页面的只读浏览器检查通过：桌面/手机均展示新卡片并回显 128 MiB，无横向溢出和页面运行错误。
- 部署前后 47 张受保护表与 5931 个文件（含全局技能）核对通过；19 个会话、3321 条活动、1 个模型配置、10 条提示词版本、1 个人格均保留。这里为本次切换时的现状，不按旧版本数量恢复用户已删除或新增的记录。`agents`/`mcp_servers` 仅排除启动自动刷新的 `updated_at`；settings、JWT、systemd、工具偏好和技能文件均保持。
- 回滚目录 `/opt/artex/_rollback-v0.2.5-yetrx.4` 保留原 `.3` 二进制、数据库 dump 和完整性记录；未自动恢复或覆盖生产数据库。
- 独立测试数据库/角色 `yetrx_file_limit_20261014`、测试二进制、临时凭据和本机操作/构建缓存在验收后清理，保留生产回滚备份。


## v0.2.5-yetrx.3：原生工作流机制与 Agent 文件交付

2026-10-14，发布前验收已完成。源码继续使用 Go `workflow.Manager`、`ChatAgent` 和现有 Norma API；本次未修改 Norma，未恢复 JavaScript runtime。

- 对照当前安装的 `@zhyx111999/pi-workflows-lean` 实现运行中消息、编辑/重排恢复、按身份与依赖复用结果、命名 thread、持久化 checkpoint、可恢复错误重试、有限额度恢复和 Git worktree 生命周期。取消/超时后的旧执行器必须真正退出；draining 期间保留槽位并禁止重叠重试/恢复。
- 子代理工具注册、延迟调用与服务端深度检查共同禁止再次编排。任务仍继承父对话当前实际模型；全局并发 4、单批最多 32，探索引擎 `workers` 独立。
- 既有实例的工具 schema 一次性升级覆盖新的控制参数，保留用户描述、嵌套参数默认值、绑定和开关；真实 PostgreSQL 测试覆盖升级、用户修改保留和幂等。
- `send_file` 将现有普通文件（最多 128 MiB）复制为当前会话的可下载附件。API 沿用 JWT，校验会话归属，支持 Range，强制附件下载；删除会话清理副本。测试覆盖中文名称、字节保留、源文件变化、无认证/错误归属、大小与类型限制。
- Linux Go 1.26.5、CGO/gcc 下：`go test -race ./workflow -count=1`、ChatAgent steering/子工具约束/thread/Persona 聚焦 race、服务器 NativeWorkflow/History/Persona/ChatFile/WorkflowSchemaUpgrade 集成 race 全部通过。集成测试使用独立数据库 `yetrx_parity_20261014`，生产数据库未写入测试数据。
- 真实 `gpt-6-astra` 的受限验收 `TestNativeWorkflowLiveParity` 通过：run `wf-69d84a923e1ba614`；2 次任务尝试，包含一次受控 recoverable 失败；总 8 次 provider 调用。按本次子会话 ID 核对 `llm_records.raw_request` 的 4 次真实 HTTP 请求均为 Astra；指令实际改变 worktree 文件，父源码保持原文；恢复复用、checkpoint JSON null 和禁止再次派发通过。主代理随后实际生成文件并调用 `send_file`，附件内容匹配。
- Windows 全仓库编译、受影响包 `go vet`、TypeScript、前端生产静态导出、新组件 Biome 检查通过；`git diff --check` 通过。
- Edge 桌面 1440px 与移动 390px 浏览器测试通过：使用生产构建和受控 API fixture，附件卡片可见，实际点击下载，认证请求头、中文名和文件字节一致，移动按钮 44px，无横向溢出或页面运行错误。后端真实 HTTP/数据库行为另由上述集成测试验证。

边界：YETRX 提供原生任务 DAG，不执行任意 pi JavaScript 工作流；恢复不保证外部副作用恰好一次。运行中消息在下一模型边界送达，提前结束则丢弃。默认共享 cwd，显式 worktree 才隔离；默认保留工作树，不自动合并。重试默认 0、最多额外 3 次；额度自动恢复有上限。长上下文耗尽后的无限续跑未验收。全仓库历史环境/基线问题见下文，不将聚焦检查描述成所有历史测试全绿。

源码提交 `ab168c54dfd2b1fa3fd24a99b177947a97f0ab81` 已推送，发布 [v0.2.5-yetrx.3](https://github.com/zhyx111999/YETRX/releases/tag/v0.2.5-yetrx.3)。GitHub Actions `38535048319` 的前端、五平台构建及 release 全部成功，Docker 按配置跳过。

- Linux amd64 ZIP SHA256：`cd725ea39e0b1fd2861081239c1a0be5c6aef17abe557817b3a437dc1ee038dc`，已按发布的 SHA256SUMS 校验；部署后二进制 SHA256：`aade363edf43ceb20f85c2151cc3252a3393edd5118c78d191e2e0f5f3a48f64`。
- 生产健康版本 `0.2.5-yetrx.3`，systemd `artex` active。下载路由拒绝匿名访问，认证请求进入附件处理器；生产工具目录 `send_file` 已启用并绑定 Agent，workflow schema 新参数已生效。
- 切换前后核对 47 张受保护表和 5904 个会话/任务/附件文件：18 个对话、3287 条活动、3 个模型配置、10 条提示词版本、1 个人格均保留；JWT、systemd 单元、已有 settings、工具绑定/开关和用户描述/默认值保持。
- 首次切换的严格整行检查因 `agents` 和 `mcp_servers` 启动时刷新 `updated_at` 而自动回退旧二进制。逐字段对照原始数据库 dump 确认 8 个 Agent、2 个 MCP 的其他字段完全相同后，仅排除这两个时间戳重新部署；最终再次与首次备份核对通过，未恢复或覆盖生产数据库。
- 回滚根目录 `/opt/artex/_rollback-v0.2.5-yetrx.3` 保留原 `.2` 二进制、部署前数据库 dump 和完整性证据；第二次切换记录在其 `attempt-2` 子目录。
- 全局 `workflow-patterns` 与旧默认内容相同，已升级；`workflow-authoring` 和 `subagents-setup` 与旧默认不同，原样保留，未覆盖用户编辑。发布包中的最新说明仍可查阅。
- 使用部署后的实际前端资源再次执行桌面/移动浏览器受控附件下载测试，结果与本地相同，无页面运行错误。真实附件存储、模型调用和认证 HTTP 行为已在独立数据库中验收。
- 临时数据库、角色、模型配置副本、服务器 Go/源码/缓存及本机操作文件在验收后清理；生产回滚备份保留。


## 全局人格与原生后台工作流模型继承：v0.2.5-yetrx.2

最终发布源码 `98f20e0`，后端基线 `0975ce8`；健康版本 `0.2.5-yetrx.2`。侧边栏和页内标题统一为「LLM 与人格」。完整对照答复与实际执行结果保存在本地忽略文档 `docs/YETRX-额外问题答复与验收.md`，该文档未纳入 Git 发布。

- GitHub Actions `38516185786` 的前端、五个平台构建及 release 全部通过；Linux amd64 发布资产按 SHA256SUMS 校验后部署，二进制 SHA256 为 `7c9f6d5f07bce3b2952cdd1d9de79a5dff799767973756d6a27618ddcbc57b11`。
- `.2` 部署前后 14 个对话、3907 条活动、3 个模型配置、10 条提示词版本和 Persona 表逐行指纹一致，JWT 密钥和 systemd 单元一致。回滚副本 `/opt/artex/_rollback-v0.2.5-yetrx.2` 保留旧二进制、数据库 dump 与指纹记录。
- Edge 线上浏览器验证导航和标题「LLM 与人格」、人格创建抽屉、字段及系统设置页，无页面运行错误。
- 独立 PostgreSQL 库执行 `TestPersonaSchemaAndCRUD`、`TestConcurrentPersonaActivationKeepsSingleActive` 和 `TestPersonaRoutesCRUDActivationAndDTO` 全部通过，无 skip；覆盖 24 次并发激活、幂等 schema、原文保留、CRUD、冲突/空白/缺失错误及 DTO。
- 相同二进制的隔离服务使用真实 `gpt-6-astra` 执行 `wf-2d3e530f052dc7fa`：两个子任务实际并行，第三个等待两项依赖成功后启动，完成后自动回灌主会话。6 次请求的模型记录与 HTTP 请求体均为 `gpt-6-astra`；人格完整正文仅出现在请求 system，未写入 transcript。
- 同一临时会话编辑激活人格后，第 7 次请求使用新人格，回复确认改变，服务未重启。临时服务、数据库、角色、工作流和会话文件均已清理。
- 先前生产临时会话 29 的宽范围审查任务长时间未收尾，已停止并删除其临时资源，不计作通过。真实 `llm_usage` 账本因缺少 session_id 未按时间猜测删除。
- 自更新回源实际遇到 GitHub 匿名 API 限流，HTTP 200 内含 error，不计为线上更新检查成功；本次通过校验 release 资产手工部署。真实长上下文耗尽/无限续跑和本版本 race 未验收。

此前完成的源码检查：

- 新增独立全局人格表、唯一名称、最多一个激活项、并发安全激活、CRUD API 和系统 LLM 设置页管理；激活人格在每次 `ChatAgent` 请求边界动态读取，保留原始用户消息、活动和 transcript。
- 原生 `subagents` 子任务沿用启动它的当前会话 ChatAgent 和模型配置；任务 `agent_key` 只决定提示词与资源可见性，不提供子任务级 `llm_profile_id` 覆盖。依赖结果、独立子任务 transcript/工作目录、后台自动回灌和 `Resume` 保留。
- 父代理提示仅在人类回合显式标记、且当前模式实际暴露 `subagents` 时追加；子任务不继承该标记。自更新版本比较支持 `vX.Y.Z-yetrx.N`，开发构建后缀仍不可比较。
- `go test ./... -run '^$'`、`go vet ./agent ./db ./selfupdate ./server ./workflow`、核心聚焦测试、`npx tsc --noEmit --pretty false` 和 Next 生产构建通过；`go test -race ./workflow` 因当前 Windows Go 环境没有可用 C 编译器未运行。
- Windows 本机没有 `ARTEX_PG_DSN` 时 PostgreSQL 分支自然跳过；后续已在服务器独立临时 PostgreSQL 库实际通过 Persona 集成测试。完整 `go test ./server` 的既有无数据库配置失败与 Windows `skillRelPath` 路径分隔符问题仍未宣称修复。

## 设置面板按钮动效：v0.2.4-yetrx.1

- 设置面板容器统一给 15 个按钮控件添加按下缩放至 95%、松开还原的 200ms 过渡，覆盖两个下拉触发器、12 个 ToggleGroupItem 和恢复默认按钮；三种 Theme Mode 选项与其他设置按钮使用同一规则。
- Edge 浏览器检查通过：15 个控件的计算样式均为 transform 200ms，模式按钮和恢复默认按钮实际按下缩放，设置齿轮旋转/缩放、面板展开和附件入口动画保持，页面无运行错误。
- 前端生产构建、TypeScript 检查、Biome 检查及全部 32 个静态页面生成通过；原生 Linux amd64 构建并部署 `v0.2.4-yetrx.1`。
- 部署保留原有对话、活动、模型配置和提示词版本；JWT 密钥与 systemd 单元未变。回滚副本：`/opt/artex/_rollback-v0.2.4-yetrx.1`。

## 设置与模式动效：v0.2.3-yetrx.1

- 设置按钮按下缩放至 90%，齿轮打开时保留顺时针 90° 旋转；设置面板使用 300ms 淡入/缩放展开。
- Theme Mode 组使用 300ms 淡入/缩放展开，Light/Dark/System 三个选项按下缩放至 95%，原有主题切换、持久化与视图过渡保持可用。
- Edge 浏览器检查通过：设置按下缩放、旋转回正、弹层 Escape、Theme Mode 展开与选择、附件 200ms 规则，页面无运行错误；Biome 检查通过。
- 前端生产构建、TypeScript 检查及全部 32 个静态页面生成通过；原生 Linux amd64 构建并部署 `v0.2.3-yetrx.1`。
- 部署保留原有 7 个对话、2323 条活动、3 个模型配置及 10 个提示词版本；JWT 密钥与 systemd 单元未变。回滚副本：`/opt/artex/_rollback-v0.2.3-yetrx.1`。

## 聊天附件与右上角动效：v0.2.2-yetrx.1

- 加号旋转和菜单出现时间从 300ms 调整为 200ms；只改附件菜单中的两处时间设置，附件上传、粘贴和选择流程保持原样。
- 设置齿轮复用原有 Button/Popover 状态：打开顺时针旋转 90°，关闭回正，过渡 300ms，并补充“界面设置”无障碍标签。
- 明暗切换保留浅色、深色、跟随系统的原有循环和持久化；支持 `document.startViewTransition` 时使用页面视图过渡，主题图标同步淡入、90°旋转和缩放 300ms；不支持该 API 或用户减少动态效果时回退到直接切换。
- Edge 浏览器检查通过：设置弹层打开/关闭、Escape、明暗/系统循环、重复点击、草稿内容保留、HTTP 页面视图过渡、浏览器不支持 API 的回退、减少动态效果以及手机视口；附件入口 200ms 规则和页面零错误通过。
- 前端生产构建、TypeScript 检查、Biome 检查及全部 32 个静态页面生成通过；原生 Linux amd64 构建并部署 `v0.2.2-yetrx.1`。
- 部署保留原有 7 个对话、2323 条活动、3 个模型配置及 10 个提示词版本；JWT 密钥与 systemd 单元未变。回滚副本：`/opt/artex/_rollback-v0.2.2-yetrx.1`。

## 附件界面更新：v0.2.1-yetrx.1

- 前端生产构建、TypeScript 检查及 32 个静态页面生成通过；新增附件菜单与剪贴板模块的 Biome 检查通过。
- Edge 浏览器验证：`＋` 顺时针旋转 90° 后显示两个上传框；再次点击、外点和 Escape 可关闭。普通文字 Ctrl+V 保留光标位置及原生撤销。
- 真实 Ctrl+V 图片、多文件选择、剪贴板文件事件及 items 回退通过；同一个剪贴板文件不重复上传。验证草稿创建会话后保留文字和附件、上传期间阻止发送、附件移除、发送字段、失败后重试。
- 手机模拟视口检查 390px 与 320px 布局、触控尺寸、图片选择器 `image/*`、无强制相机参数，以及减少动态效果设置。没有进行实体手机相册界面自动化；最终原生选择界面由手机系统与浏览器决定。
- 原生 Linux amd64 构建并部署 `v0.2.1-yetrx.1`。线上浏览器通过真实接口上传图片和粘贴文件，服务端落盘字节校验一致，页面无运行错误；未调用真实模型。临时验证对话和上传目录已清理。
- 部署保留原有 7 个对话、2323 条活动、3 个模型配置及 10 个提示词版本；逐行指纹校验一致，JWT 密钥与 systemd 单元未变。回滚副本：`/opt/artex/_rollback-attachments-20261013`。

## 原生架构重建：v0.2.0-yetrx.1

本轮从上游 `a9d11cb` 重建原生版本，未沿用旧版 pi 执行分支。

已完成：

- 前端生产构建、TypeScript 检查和全部 32 个静态页面生成。
- `go test ./workflow`：并行与依赖、并发上限、停止/恢复、重启中断、状态保存、会话归属和结果截取。
- 独立 PostgreSQL 16 上的原生资源回归：内置 Agent 与自定义模式共用同一技能、MCP、函数工具；编辑、禁用和绑定变化都按原规则生效；使用真实 ChatAgent 和假模型执行真实函数工具/MCP 调用。
- 原生后台接入回归：子任务使用全局资源，前台轮次占用期间等待回传，结束后自动汇总并保存；停止后不再启动排队回复；已取消的请求不能创建后台任务。
- 旧版会话衔接回归：活动账本不改写，保留上下文和原 transcript 副本，旧工具调用不重放，同一会话不会二次导入。
- 8 组数据库迁移测试：新库、原上游和旧内嵌版本升级、用户编辑保留、同名冲突、失败回滚/重试、并发启动/提示词保存。只使用显式本地测试 DSN，并为各用例创建独立数据库。
- 相关认证、全局工具装配与技能回归测试。
- Linux amd64 原生可执行文件，静态前端内嵌，CGO 关闭。
- `go vet ./server ./workflow` 通过；工作流模块在独立工作树完成 `-race` 验证。

线上部署 `v0.2.0-yetrx.1` 后另做了真实配置模型验证：Agent 读取全局技能并调用全局函数，子任务通过原生 Bash 执行后自动回传；编辑同一个函数后，自定义模式读取到更新，并调用同一全局技能。临时模式、工具、对话及执行文件已清理。浏览器检查登录、图标、默认模式与页面标题，通过且无页面运行错误。

部署前后的原有对话、活动、模型配置和提示词版本逐行校验保持一致；原技能/MCP可见性记录、JWT 密钥和 systemd 单元保持一致。旧 token 仍可用，原生服务自动重启、自启与公网版本检查通过。应用目录已移除旧内嵌运行时，旧版本保留在回滚备份。

前述本地测试未使用生产数据库或真实模型密钥；线上验证只使用临时测试资源。原生模式不需要 pi SDK、模型中继或 Node Agent 进程。

完整 Windows 测试集仍存在上游已知失败，包括证据目录同步权限、技能路径分隔符和部分临时目录清理问题；公司范围测试在未修改的上游也失败。本轮针对修改范围运行验证，不宣称全部上游测试通过。
