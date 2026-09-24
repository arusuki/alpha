package rootless

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const testLabel = "io.rootless-docker.cli-test"
const testHost = "unix:///var/run/docker.sock"

func (m *manager) testLookup(name string) (string, error) {
	r, err := m.testDocker("container", "ls", "--all", "--no-trunc", "--format", "{{.ID}} {{.Names}}")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(r.Out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != name {
			continue
		}
		id := fields[0]
		r, err = m.testDocker("container", "inspect", "--format", `{{index .Config.Labels "`+testLabel+`"}}`, id)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(r.Out) != "1" {
			return "", fmt.Errorf("同名容器 %s 不是本工具创建的，拒绝操作。", name)
		}
		return id, nil
	}
	return "", nil
}
func (m *manager) testUp(u account, name string) (string, error) {
	r, err := m.docker(u, "", "info", "--format", "{{.ID}}")
	if err != nil {
		return "", err
	}
	expected := strings.TrimSpace(r.Out)
	if expected == "" {
		return "", errors.New("无法获取 rootless daemon ID；请先执行 rootless-docker init。")
	}
	cid, err := m.testLookup(name)
	if err != nil {
		return "", err
	}
	if cid == "" {
		image := os.Getenv("ROOTLESS_TEST_IMAGE")
		if image == "" {
			image = "docker:28-cli"
		}
		r, err = m.testDocker("run", "--detach", "--name", name, "--label", testLabel+"=1", "--user", "0:0", "--env", "DOCKER_HOST="+testHost, "--env", "DOCKER_TLS_CERTDIR=", "--entrypoint", "/bin/sh", image, "-c", "exec tail -f /dev/null")
		if err != nil {
			return "", err
		}
		cid = strings.TrimSpace(r.Out)
	} else {
		r, err = m.testDocker("container", "inspect", "--format", "{{.State.Running}}", cid)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(r.Out) != "true" {
			if _, err = m.testDocker("start", cid); err != nil {
				return "", err
			}
		}
	}
	if err = m.add(u, options{Container: cid, Host: testHost, SocketPath: "/var/run/docker.sock"}); err != nil {
		fmt.Fprintf(m.errOut, "挂载失败。修复后重新执行 test up；也可用 test cleanup 删除测试容器 %s。\n", name)
		return "", err
	}
	r, err = m.testDocker("exec", "--user", "0", cid, "docker", "--host", testHost, "info", "--format", "{{.ID}}")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(r.Out) != expected {
		return "", errors.New("容器内连接的 daemon ID 与 rootless daemon 不一致。")
	}
	fmt.Fprintf(m.out, "测试容器 %s 已就绪，已确认连接到 rootless daemon：%s\n", name, expected)
	r, err = m.testDocker("exec", "--user", "0", cid, "docker", "--host", testHost, "info", "--format", "Server={{.ServerVersion}} Root={{.DockerRootDir}} Security={{json .SecurityOptions}}")
	if err != nil {
		return "", err
	}
	fmt.Fprint(m.out, r.Out)
	return cid, nil
}
func (m *manager) testContainer(args []string) error {
	name := "rootless-cli-test"
	if len(args) == 2 {
		name = args[1]
	}
	r, err := m.testDocker("info", "--format", "{{json .SecurityOptions}}")
	if err != nil {
		return err
	}
	if strings.Contains(r.Out, "rootless") {
		return errors.New(testHost + " 必须是宿主机 rootful Docker。")
	}
	if args[0] == "cleanup" {
		cid, err := m.testLookup(name)
		if err != nil {
			return err
		}
		if cid == "" {
			fmt.Fprintf(m.out, "测试容器 %s 不存在，无需清理。\n", name)
			return nil
		}
		r, err = m.testDocker("rm", "--force", "--volumes", cid)
		if err != nil {
			return err
		}
		fmt.Fprint(m.out, r.Out)
		fmt.Fprintf(m.out, "已删除测试容器 %s。\n", name)
		return nil
	}
	u, err := lookupAccount()
	if err != nil {
		return err
	}
	cid, err := m.testUp(u, name)
	if err != nil {
		return err
	}
	switch args[0] {
	case "exec":
		fmt.Fprintln(m.out, "已进入测试终端，可执行 docker version、docker ps、docker run --rm hello-world；exit 退出。")
		cmd := []string{"docker", "--host", testHost, "exec", "-i"}
		_, e1 := unix.IoctlGetTermios(0, unix.TCGETS)
		_, e2 := unix.IoctlGetTermios(1, unix.TCGETS)
		if e1 == nil && e2 == nil {
			cmd = append(cmd, "-t")
		}
		cmd = append(cmd, "--user", "0", "--env", "DOCKER_HOST="+testHost, "--env", "DOCKER_CONTEXT=", "--env", "DOCKER_TLS_VERIFY=", "--env", "DOCKER_TLS=", "--env", "DOCKER_CERT_PATH=", "--env", "DOCKER_API_VERSION=", cid, "/bin/sh")
		return execCommand(cmd, rootDockerEnv)
	case "check":
		for _, command := range [][]string{{"version"}, {"run", "--rm", "hello-world"}} {
			cmd := append([]string{"docker", "--host", testHost, "exec", "--user", "0", cid, "docker", "--host", testHost}, command...)
			r, err := m.run(cmd, rootDockerEnv, 0, true)
			fmt.Fprint(m.out, r.Out)
			fmt.Fprint(m.errOut, r.Err)
			if err != nil {
				return err
			}
		}
		fmt.Fprintln(m.out, "Docker CLI 连接和创建容器测试通过。")
	}
	return nil
}

func (m *manager) testDocker(args ...string) (result, error) {
	return m.run(append([]string{"docker", "--host", testHost}, args...), rootDockerEnv, 0, true)
}
