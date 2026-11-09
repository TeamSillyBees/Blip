package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func noArgs(command *cobra.Command, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("%s 不接受参数或子命令 %q；运行 %s --help 查看用法", command.CommandPath(), args[0], command.CommandPath())
	}
	return nil
}

func oneEventVersionArg(command *cobra.Command, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("请指定一个事件协议版本（1 或 2）；运行 %s --help 查看用法", command.CommandPath())
	}
	return nil
}

func requiredFlagsError(command *cobra.Command) error {
	var missing []string
	command.Flags().VisitAll(func(flag *pflag.Flag) {
		required := flag.Annotations[cobra.BashCompOneRequiredFlag]
		if len(required) > 0 && required[0] == "true" && !flag.Changed {
			missing = append(missing, "--"+flag.Name)
		}
	})
	if len(missing) > 0 {
		return fmt.Errorf("缺少必填选项：%s；运行 %s --help 查看用法", strings.Join(missing, "、"), command.CommandPath())
	}
	return nil
}

// pflag exposes structured parse errors; translate their labels without
// replacing text inside user-supplied names or values.
func flagError(command *cobra.Command, err error) error {
	var message string
	switch parseErr := err.(type) {
	case *pflag.NotExistError:
		prefix := "--"
		if parseErr.GetSpecifiedShortnames() != "" {
			prefix = "-"
		}
		message = "未知选项：" + prefix + parseErr.GetSpecifiedName()
	case *pflag.ValueRequiredError:
		message = "选项 --" + parseErr.GetFlag().Name + " 需要一个值"
	case *pflag.InvalidValueError:
		message = fmt.Sprintf("选项 --%s 的值 %q 无效", parseErr.GetFlag().Name, parseErr.GetValue())
		switch parseErr.GetFlag().Value.Type() {
		case "bool":
			message += "，请使用 true 或 false"
		case "int":
			message += "，请输入有效范围内的整数"
		}
	case *pflag.InvalidSyntaxError:
		message = "选项格式无效：" + parseErr.GetSpecifiedFlag()
	default:
		return err
	}
	return &localizedError{message: message + "；运行 " + command.CommandPath() + " --help 查看用法", cause: err}
}

type localizedError struct {
	message string
	cause   error
}

func (e *localizedError) Error() string { return e.message }
func (e *localizedError) Unwrap() error { return e.cause }
