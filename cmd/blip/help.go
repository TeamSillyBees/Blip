package main

import (
	"fmt"
	"strings"
	"text/template"

	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Render labels separately from command names, paths and default values so
// localization never rewrites something the user needs to copy or execute.
var chineseUsage = template.Must(template.New("usage").Funcs(template.FuncMap{
	"rpad": func(text string, width int) string {
		return text + strings.Repeat(" ", max(0, width-ansi.StringWidth(text)))
	},
}).Parse(`用法：{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [子命令]{{end}}{{if .Aliases}}

别名：
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

示例：
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if .Groups}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding}} {{.Short}}{{end}}{{end}}{{end}}{{else}}

可用命令：{{range $cmds}}{{if or .IsAvailableCommand (eq .Name "help")}}
  {{rpad .Name .NamePadding}} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

选项：
{{.LocalFlagUsages}}{{end}}{{if .HasAvailableInheritedFlags}}

全局选项：
{{.InheritedFlagUsages}}{{end}}{{if .HasAvailableSubCommands}}

运行 "{{.CommandPath}} [子命令] --help" 查看子命令的详细说明。{{end}}
`))

type commandHelp struct{ *cobra.Command }

func (h commandHelp) UseLine() string {
	line := h.Command.UseLine()
	if strings.HasSuffix(line, " [flags]") {
		return strings.TrimSuffix(line, " [flags]") + " [选项]"
	}
	return line
}

func (h commandHelp) LocalFlagUsages() string { return chineseFlagUsages(h.LocalFlags()) }
func (h commandHelp) InheritedFlagUsages() string {
	return chineseFlagUsages(h.InheritedFlags())
}

func chineseFlagUsages(flags *pflag.FlagSet) string {
	type row struct{ label, description string }
	var rows []row
	width := 0
	flags.VisitAll(func(flag *pflag.Flag) {
		if flag.Hidden {
			return
		}
		label := "      --" + flag.Name
		if flag.Shorthand != "" && flag.ShorthandDeprecated == "" {
			label = "  -" + flag.Shorthand + ", --" + flag.Name
		}
		valueName, description := pflag.UnquoteUsage(flag)
		switch valueName {
		case "string":
			valueName = "字符串"
		case "int":
			valueName = "整数"
		}
		if valueName != "" {
			label += " " + valueName
		}
		if value := flag.NoOptDefVal; value != "" && !(flag.Value.Type() == "bool" && value == "true") {
			if flag.Value.Type() == "string" {
				label += fmt.Sprintf("[=%q]", value)
			} else {
				label += "[=" + value + "]"
			}
		}
		// Blip currently uses string, integer and boolean options. Preserve
		// defaults for other value types verbatim if more options are added.
		showDefault := flag.DefValue != ""
		value := flag.DefValue
		switch flag.Value.Type() {
		case "string":
			value = fmt.Sprintf("%q", value)
		case "bool":
			showDefault = showDefault && value != "false"
		case "int":
			showDefault = showDefault && value != "0"
		}
		if showDefault {
			description += "（默认值：" + value + "）"
		}
		if flag.Deprecated != "" {
			description += "（已弃用：" + flag.Deprecated + "）"
		}
		width = max(width, ansi.StringWidth(label))
		rows = append(rows, row{label, description})
	})
	var output strings.Builder
	for _, row := range rows {
		padding := strings.Repeat(" ", width-ansi.StringWidth(row.label)+3)
		description := strings.ReplaceAll(row.description, "\n", "\n"+strings.Repeat(" ", width+3))
		fmt.Fprintln(&output, row.label+padding+description)
	}
	return strings.TrimRight(output.String(), "\n")
}

func localizeHelp(root *cobra.Command) {
	root.SetUsageFunc(func(command *cobra.Command) error {
		// LocalFlags merges persistent flags before UseLine is rendered.
		command.LocalFlags()
		return chineseUsage.Execute(command.OutOrStderr(), commandHelp{command})
	})
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	for _, command := range root.Commands() {
		switch command.Name() {
		case "help":
			command.Use = "help [命令]"
			command.Short = "查看命令的帮助信息"
			command.Long = "查看任意命令的详细说明。\n例如：blip help service install 或 blip service install --help。"
			command.Run = nil
			command.RunE = func(command *cobra.Command, args []string) error {
				target, remaining, err := root.Find(args)
				if err != nil || len(remaining) != 0 {
					return fmt.Errorf("找不到帮助主题 %q；运行 blip help 查看可用命令", strings.Join(args, " "))
				}
				target.SetContext(command.Context())
				return target.Help()
			}
		case "completion":
			command.Short = "生成命令行自动补全脚本"
			command.Long = "为指定的命令行环境生成 Blip 自动补全脚本。\n运行 blip completion [子命令] --help 查看加载方法。"
			command.Args = noArgs
			for _, shell := range command.Commands() {
				shell.Short = "生成 " + shell.Name() + " 自动补全脚本"
				shell.Long = completionHelp[shell.Name()]
				shell.Args = noArgs
				if flag := shell.Flags().Lookup("no-descriptions"); flag != nil {
					flag.Usage = "不在补全候选项中显示说明"
				}
			}
		}
	}
	localizeBuiltinFlags(root)
}

func localizeBuiltinFlags(command *cobra.Command) {
	command.InitDefaultHelpFlag()
	command.InitDefaultVersionFlag()
	if flag := command.Flags().Lookup("help"); flag != nil {
		flag.Usage = "查看帮助信息"
	}
	if flag := command.Flags().Lookup("version"); flag != nil {
		flag.Usage = "显示版本号"
	}
	for _, child := range command.Commands() {
		localizeBuiltinFlags(child)
	}
}

var completionHelp = map[string]string{
	"bash": `生成 bash 自动补全脚本，需要先通过系统包管理器安装 bash-completion。

在当前会话中加载：
  source <(blip completion bash)

在后续会话中自动加载，按系统选择脚本保存位置：
  Linux：blip completion bash > /etc/bash_completion.d/blip
  macOS：blip completion bash > "$(brew --prefix)/etc/bash_completion.d/blip"

保存后重新打开终端。`,
	"zsh": `生成 zsh 自动补全脚本。

如果尚未启用补全，在 ~/.zshrc 中添加：
  autoload -U compinit; compinit

在当前会话中加载：
  source <(blip completion zsh)

在后续会话中自动加载，将脚本保存到补全搜索目录：
  blip completion zsh > "${fpath[1]}/_blip"

保存后重新打开终端。`,
	"fish": `生成 fish 自动补全脚本。

在当前会话中加载：
  blip completion fish | source

在后续会话中自动加载：
  mkdir -p ~/.config/fish/completions
  blip completion fish > ~/.config/fish/completions/blip.fish

保存后重新打开终端。`,
	"powershell": `生成 PowerShell 自动补全脚本。

在当前会话中加载：
  blip completion powershell | Out-String | Invoke-Expression

如需在后续会话中自动加载，将上面的命令添加到 PowerShell 配置文件 $PROFILE 中。`,
}
