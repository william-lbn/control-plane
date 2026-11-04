package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/william-lbn/control-plane/api/internal/control"
)

func main() {
	input := flag.String("input", "", "原版本化managed SQL route输入文件（不含凭据或Native事实）")
	kubeconfig := flag.String("kubeconfig", "", "既有Kube客户端配置；空时沿原in-cluster配置")
	timeout := flag.Duration("timeout", 45*time.Second, "本次有界注册总时限，最大45秒")
	flag.Parse()
	if flag.NArg() != 0 || *input == "" || *timeout <= 0 || *timeout > 45*time.Second {
		fmt.Fprintln(os.Stderr, "managed_sql_route_invalid_input")
		os.Exit(2)
	}
	f, err := os.Open(*input)
	if err != nil {
		fmt.Fprintln(os.Stderr, "managed_sql_route_input_unavailable")
		os.Exit(2)
	}
	raw, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	f.Close()
	if err != nil || len(raw) > 64<<10 {
		fmt.Fprintln(os.Stderr, "managed_sql_route_invalid_input")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	receipt, err := control.PublishManagedSQLRoute(ctx, *kubeconfig, raw)
	if err != nil {
		// 不打印底层Kube/SQL/HTTP错误正文；其中可能含私有数据。
		fmt.Fprintln(os.Stderr, control.ManagedSQLRouteFailureCode(err))
		os.Exit(1)
	}
	if json.NewEncoder(os.Stdout).Encode(receipt) != nil {
		os.Exit(1)
	}
}
