package bastion

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strconv"
)

func shareServiceCLI(ctx context.Context, action string, args []string, out io.Writer) error {
	p := flag.NewFlagSet("project-alpha share-node "+action, flag.ContinueOnError)
	p.SetOutput(out)
	lines, follow := 50, false
	p.Usage = func() {
		if action == "status" {
			fmt.Fprint(out, "用法：\n  project-alpha share-node status\n\n查看 project-alpha-share-node.service 的 systemd 状态，不分页、不截断长行。\n服务未运行或不存在时仍输出状态，并返回错误。\n")
			return
		}
		fmt.Fprint(out, `用法：
  project-alpha share-node log [-n 行数] [-f]

查看 project-alpha-share-node.service 的 journal 日志，默认最近 50 行，不分页。
需要 journal 读取权限；权限不足时使用 sudo。

示例：
  project-alpha share-node log
  sudo project-alpha share-node log -n 100 -f

选项：
`)
		p.PrintDefaults()
	}
	if action == "log" {
		p.IntVar(&lines, "n", 50, "显示最近多少行日志（非负整数）")
		p.IntVar(&lines, "lines", 50, "同 -n")
		p.BoolVar(&follow, "f", false, "持续跟踪日志，Ctrl+C 退出")
		p.BoolVar(&follow, "follow", false, "同 -f")
	}
	if err := p.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if p.NArg() != 0 {
		return fmt.Errorf("share-node %s 不接受额外参数", action)
	}
	if lines < 0 {
		return fmt.Errorf("日志行数必须为非负整数")
	}
	name := "systemctl"
	commandArgs := []string{"status", proxyUnitName, "--no-pager", "--full"}
	if action == "log" {
		name = "journalctl"
		commandArgs = []string{"-u", proxyUnitName, "--no-pager", "-n", strconv.Itoa(lines)}
		if follow {
			commandArgs = append(commandArgs, "--follow")
		}
	}
	cmd := exec.CommandContext(ctx, name, commandArgs...)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("查看 share node %s 失败（%s）：%w", action, name, err)
	}
	return nil
}
