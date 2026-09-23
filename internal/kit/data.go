package kit

import (
	"os"
	"path/filepath"
)

// ResolveDataPath 数据文件路径解析：优先 data/ 子目录（可执行文件目录，其次工作目录），
// 兼容历史根目录存放；均不存在时默认写到 data/ 子目录并自动创建目录。
// go run 运行时编译产物在临时目录，此时应回退到工作目录（项目根）的 data/。
//
// 创建目录时若首选位置(exe 目录 / 工作目录)不可写(例如 exe 放在 C:\Program Files\)，
// 回退到用户配置目录(os.UserConfigDir: Windows %LOCALAPPDATA%, 其他平台 ~/.config)
// 下的 free-router/data；三候选均失败则按惯例返回 exeDir/data 路径，
// 后续各落盘点自行报错。
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
	// 避免把 exe 放进 C:\ Program Files 后数据无法落盘而毫无报错。
	for _, base := range []string{exeDir, pwd} {
		if base == "" {
			continue
		}
		p := filepath.Join(base, "data")
		if err := os.MkdirAll(p, 0755); err == nil {
			return filepath.Join(p, filename)
		}
	}
	if cfgDir, err := os.UserConfigDir(); err == nil {
		p := filepath.Join(cfgDir, "free-router", "data")
		if err := os.MkdirAll(p, 0755); err == nil {
			return filepath.Join(p, filename)
		}
	}
	// 三候选(含用户配置目录)全部建不出来: 按惯例返回 exeDir/data 路径,
	// 写入失败由各自落盘点报错 —— 该兜底几乎不可达, 不再维护全局错误槽位
	// (此前 LastDataPathError 只写不读, 且无锁裸写与并发读构成撕裂风险)。
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
