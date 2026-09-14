package kit

import (
	"os"
	"path/filepath"
)

// LastDataPathError 记录最近一次 ResolveDataPath 创建数据目录失败的原因。
// 上层(如桌面模式启动)可在初始化后检查 kit.DataPathError()：数据目录不可写时
// 给出可见提示——GUI 无控制台, 否则"托盘亮着、面板能开、但日志和统计全空、
// 没有任何报错"。无失败时为 nil。
var LastDataPathError error

// DataPathError 返回最近一次数据目录创建失败的 error(可空)。
func DataPathError() error { return LastDataPathError }

// ResolveDataPath 数据文件路径解析：优先 data/ 子目录（可执行文件目录，其次工作目录），
// 兼容历史根目录存放；均不存在时默认写到 data/ 子目录并自动创建目录。
// go run 运行时编译产物在临时目录，此时应回退到工作目录（项目根）的 data/。
//
// 创建目录时若首选位置(exe 目录 / 工作目录)不可写(例如 exe 放在 C:\Program Files\)，
// 回退到用户配置目录(os.UserConfigDir: Windows %LOCALAPPDATA%, 其他平台 ~/.config)
// 下的 cline-proxy/data；仍失败则记录错误到 LastDataPathError 供上层感知。
func ResolveDataPath(filename string) string {
	exeDir, pwd := "", ""
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	if wd, err := os.Getwd(); err == nil {
		pwd = wd
	}
	candidates := []string{}
	if exeDir != "" {
		candidates = append(candidates, filepath.Join(exeDir, "data", filename))
	}
	if pwd != "" {
		candidates = append(candidates, filepath.Join(pwd, "data", filename))
	}
	if exeDir != "" {
		candidates = append(candidates, filepath.Join(exeDir, filename))
	}
	if pwd != "" {
		candidates = append(candidates, filepath.Join(pwd, filename))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	// 均不存在：选择可写的数据目录并创建。优先 exeDir/data(惯例)，
	// 其次 pwd/data(go run 场景)；两者都不可写时回退到用户配置目录，
	// 避免把 exe 放进 C:\Program Files\ 后数据无法落盘而毫无报错。
	var mkErr error
	for _, base := range []string{exeDir, pwd} {
		if base == "" {
			continue
		}
		p := filepath.Join(base, "data")
		if err := os.MkdirAll(p, 0755); err == nil {
			return filepath.Join(p, filename)
		} else if mkErr == nil {
			mkErr = err
		}
	}
	if cfgDir, err := os.UserConfigDir(); err == nil {
		p := filepath.Join(cfgDir, "cline-proxy", "data")
		if mkErr2 := os.MkdirAll(p, 0755); mkErr2 == nil {
			return filepath.Join(p, filename)
		} else if mkErr == nil {
			mkErr = mkErr2
		}
	}
	// 所有候选目录都建不出来：记下错误供上层感知，仍按惯例返回 exeDir/data
	// 路径(上层若读取 kit.DataPathError() 可拿到原因，例如触发启动失败弹窗)。
	LastDataPathError = mkErr
	if exeDir != "" {
		return filepath.Join(exeDir, "data", filename)
	}
	return filepath.Join(pwd, "data", filename)
}

// FileExists 判断文件是否存在。
func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
