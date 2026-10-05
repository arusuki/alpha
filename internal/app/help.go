package app

import (
	"fmt"
	"io"
)

func printHelp(out io.Writer) {
	fmt.Fprint(out, `用法：
  project-alpha [serve] [选项]
  project-alpha <子命令> [选项]

子命令：
  serve              启动总控（默认）、worker 或公网 registry
  share-node         初始化分享节点；status 查看服务状态，log 查看日志；--uninstall 卸载
  containers import  导入已有 Docker 容器
  scan               扫描存储，导出 JSON 快照
  process            采集或回放容器进程，导出 JSON 进程树

常用参数：
  --control / --worker / --registry  选择服务角色，默认总控
  --data-dir DIR                    服务数据目录，默认 data
  --host ADDR --port PORT            监听地址与端口，默认 127.0.0.1:8765
  --version                         显示版本
  -h, --help                        显示帮助

详细帮助：project-alpha <子命令> --help
完整服务参数：project-alpha serve --help
`)
}

const serveHelp = `用法：
  project-alpha serve [选项]
  project-alpha [选项]

默认启动集群总控；--worker 启动 API-only 计算节点，--registry 启动公网注册服务。
三种角色互斥，总控和各节点须使用独立数据目录。

示例：
  project-alpha serve --control --data-dir ./control-data
  project-alpha serve --worker --data-dir ./node-data --host 0.0.0.0 --port 8766
  project-alpha serve --registry --data-dir ./registry-data --host 0.0.0.0

选项：
`

func internalHelp(args []string, out io.Writer, command, synopsis, description string) bool {
	if len(args) != 1 || args[0] != "-h" && args[0] != "--help" {
		return false
	}
	fmt.Fprintf(out, "用法：\n  project-alpha %s%s\n\n%s\n\n此命令供程序内部调用。\n", command, synopsis, description)
	return true
}
