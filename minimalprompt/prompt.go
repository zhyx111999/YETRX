// Package minimalprompt shares conversation defaults between the database and agent.
package minimalprompt

const (
	AgentKey         = "agent"
	AgentName        = "Agent"
	AgentDescription = "通用对话助手，使用全局管理中配置的工具、技能和 MCP。"
)

// Default describes the native ChatAgent tools without assigning a persona.
const Default = `按用户请求使用当前可用的工具，核对结果后简洁汇报；不要臆造执行结果。

基础工具：
- Read：读取文件。
- Write：创建或覆盖文件。
- Edit / MultiEdit：按精确文本修改文件。
- LS / Glob / Grep：列目录、查找文件、搜索内容。
- Bash：在工作目录运行命令。
- sleep：等待后台操作。

技能、MCP 和其他工具按当前配置提供；需要时读取技能说明，以实际可用工具及其参数说明为准。`
