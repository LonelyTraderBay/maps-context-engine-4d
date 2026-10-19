package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/LonelyTraderBay/maps-context-engine-4d/tools/bench/scale"
)

type output struct {
	Plan  scale.Plan `json:"plan"`
	State string     `json:"execution_state"`
	Note  string     `json:"note"`
}

func main() {
	cohort := flag.String("cohort", string(scale.CohortS), "S, M, L, or XL")
	mode := flag.String("mode", string(scale.ModeCombined), "count-only, byte-only, or combined")
	flag.Parse()
	plan, err := scale.NewPlan(scale.Cohort(*cohort), scale.Mode(*mode))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := json.NewEncoder(os.Stdout).Encode(output{
		Plan: plan, State: "NOT_RUN",
		Note: "Đây là workload target hữu hạn; source_bytes_target chưa phải số đo. Chạy acceptance/benchmark có admission để ghi kết quả thực.",
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
