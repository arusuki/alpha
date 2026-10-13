package chatcli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"project-alpha/internal/buildinfo"
)

const api = "/api/member-assistant"

func readLine(reader *bufio.Reader) (string, error) {
	var data []byte
	for {
		part, more, err := reader.ReadLine()
		if err != nil {
			return "", err
		}
		data = append(data, part...)
		if len(data) > 32768 {
			return "", fmt.Errorf("输入过长")
		}
		if !more {
			return string(data), nil
		}
	}
}

func readLineContext(ctx context.Context, reader *bufio.Reader) (string, error) {
	type line struct {
		text string
		err  error
	}
	ready := make(chan line, 1)
	go func() { text, err := readLine(reader); ready <- line{text, err} }()
	select {
	case v := <-ready:
		return v.text, v.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func Run(ctx context.Context, args []string, in io.Reader, out, status io.Writer) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return run(ctx, args, in, out, status, executable)
}

func run(ctx context.Context, args []string, in io.Reader, out, status io.Writer, executable string) error {
	name := filepath.Base(os.Args[0])
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(status)
	server := flags.String("server", "", "总控或分享入口 HTTP(S) 地址；默认使用下发配置或保存的登录地址")
	username := flags.String("user", "", "注册时的使用者标识；默认使用下发配置或保存的登录账号")
	statePath := flags.String("config", "", "登录文件路径；默认用户配置目录下 alpha-chat/session.json")
	passwordStdin := flags.Bool("password-stdin", false, "从标准输入第一行读取密码并重新登录")
	message := flags.String("message", "", "发送单条问题后退出；默认进入交互对话")
	session := flags.String("session", "", "继续指定的历史会话")
	list := flags.Bool("list", false, "列出本人的历史会话")
	logout := flags.Bool("logout", false, "注销并删除保存的访问 token")
	version := flags.Bool("version", false, "显示版本")
	flags.Usage = func() {
		fmt.Fprintf(status, "用法：%s [选项]\n容器下发安装已配置地址和账号，直接运行即可登录。\n输入 /exit 退出，/new 新建会话。Ctrl-C 取消请求并退出。\n", name)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *version {
		fmt.Fprintln(out, buildinfo.String("alpha-chat"))
		return nil
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("不接受子命令或位置参数，请使用 --message 或交互输入")
	}
	if *session != "" && !sessionPattern.MatchString(*session) {
		return fmt.Errorf("会话 ID 格式无效")
	}
	if (*list && (*logout || *message != "" || *session != "")) || (*logout && (*message != "" || *session != "" || *passwordStdin)) {
		return fmt.Errorf("--list、--logout 和对话选项不能混用")
	}
	if *statePath == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		*statePath = filepath.Join(dir, "alpha-chat", "session.json")
	}
	defaults, err := readDefaults(executable)
	if err != nil {
		return err
	}
	saved, err := readState(*statePath)
	if err != nil {
		return err
	}
	if *server == "" {
		*server = defaults.Server
	}
	if *username == "" {
		*username = defaults.Username
	}
	if *server == "" {
		*server = saved.Server
	}
	if *username == "" {
		*username = saved.Username
	}
	endpoint, err := serverURL(*server)
	if err != nil {
		return err
	}
	if !usernamePattern.MatchString(*username) {
		return fmt.Errorf("请用 --user 指定注册时的使用者标识")
	}
	c := newClient(endpoint)
	defer c.http.CloseIdleConnections()
	if saved.Server == endpoint && saved.Username == *username {
		c.token = saved.Token
	}
	if *logout {
		if c.token == "" {
			return fmt.Errorf("该服务器和使用者没有保存的登录")
		}
		err := c.request(ctx, "POST", api+"/logout", map[string]any{}, nil)
		var auth *apiError
		if err != nil && !(errors.As(err, &auth) && auth.status == 401) {
			return err
		}
		if err := os.Remove(*statePath); err != nil {
			return err
		}
		fmt.Fprintln(status, "已退出登录")
		return nil
	}
	reader := bufio.NewReader(in)
	if c.token != "" && !*passwordStdin {
		var me struct{ ID, Username string }
		err := c.request(ctx, "GET", api+"/me", nil, &me)
		var auth *apiError
		if errors.As(err, &auth) && auth.status == 401 {
			c.token = ""
		} else if err != nil {
			return err
		} else if me.Username != *username || me.ID == "" {
			return fmt.Errorf("服务器返回的身份与登录账号不匹配")
		}
	}
	if c.token == "" || *passwordStdin {
		c.token = ""
		var password string
		if *passwordStdin {
			password, err = readLineContext(ctx, reader)
		} else {
			password, err = terminalPassword(ctx)
		}
		if err != nil {
			return err
		}
		var login struct {
			Token string `json:"session_token"`
		}
		err = c.request(ctx, "POST", "/api/status/"+*username+"/login", map[string]string{"password": password}, &login)
		password = ""
		if err != nil {
			return err
		}
		if !tokenPattern.MatchString(login.Token) {
			return fmt.Errorf("服务器返回的访问 token 格式无效")
		}
		c.token = login.Token
		if err := saveState(*statePath, loginState{endpoint, *username, c.token}); err != nil {
			return err
		}
	}
	if *list {
		var history struct{ Sessions []struct{ ID, Title string } }
		if err := c.request(ctx, "GET", api+"/sessions", nil, &history); err != nil {
			return err
		}
		for _, s := range history.Sessions {
			fmt.Fprintf(out, "%s\t%s\n", safeText(s.ID), safeText(s.Title))
		}
		return nil
	}
	type conversation struct {
		ID       string
		Messages []struct{ Role, Content string }
	}
	if *session != "" {
		var previous conversation
		if err := c.request(ctx, "GET", api+"/sessions/"+*session, nil, &previous); err != nil {
			return err
		}
		if *message == "" {
			for _, m := range previous.Messages {
				fmt.Fprintf(out, "%s: %s\n\n", safeText(m.Role), safeText(m.Content))
			}
		}
	}
	send := func(text string) error {
		path := api + "/sessions"
		if *session != "" {
			path += "/" + *session + "/messages"
		}
		var result conversation
		fmt.Fprintln(status, "正在处理…")
		if err := c.request(ctx, "POST", path, map[string]string{"message": text}, &result); err != nil {
			return err
		}
		if !sessionPattern.MatchString(result.ID) || len(result.Messages) == 0 || result.Messages[len(result.Messages)-1].Role != "assistant" {
			return fmt.Errorf("服务器返回的对话格式无效")
		}
		*session = result.ID
		fmt.Fprintf(status, "会话: %s\n", result.ID)
		fmt.Fprintln(out, safeText(result.Messages[len(result.Messages)-1].Content))
		return nil
	}
	if *message != "" {
		return send(*message)
	}
	fmt.Fprintf(status, "已登录 %s。输入 /exit 退出，/new 新建会话。\n", *username)
	for {
		fmt.Fprint(status, "> ")
		line, err := readLineContext(ctx, reader)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		line = strings.TrimSpace(line)
		switch line {
		case "":
			continue
		case "/exit":
			return nil
		case "/new":
			*session = ""
			continue
		}
		if err := send(line); err != nil {
			return err
		}
	}
}
