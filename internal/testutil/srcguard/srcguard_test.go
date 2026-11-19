package srcguard

import "testing"

func TestViolations(t *testing.T) {
	cases := []struct {
		name, body string
		want       int
	}{
		{"stderr", `var _ = os.Stderr`, 1},
		{"stdout", `var _ = os.Stdout`, 1},
		{"log print", `func f() { log.Printf("x") }`, 1},
		{"log fatal", `func f() { log.Fatalf("x") }`, 1},
		{"log panic", `func f() { log.Panicln("x") }`, 1},
		{"print builtin", `func f() { print("x") }`, 1},
		{"println builtin", `func f() { println("x") }`, 1},
		{"comment and string only", "// os.Stderr log.Printf print(1)\nvar s = \"os.Stderr log.Print\"", 0},
		{"slog info", `func f() { slog.Info("x") }`, 1},
		{"slog error ctx", `func f(c context.Context) { slog.ErrorContext(c, "x") }`, 1},
		{"slog log", `func f() { slog.Log(nil, slog.LevelInfo, "x"); slog.LogAttrs(nil, 0, "x") }`, 2},
		{"slog default", `func f() { slog.Default().Info("x") }`, 1},
		{"slog allowed", `func f(l *slog.Logger) { l.Info("x", slog.String("a", "b")); _ = slog.New(slog.DiscardHandler); _ = slog.NewTextHandler(nil, nil); slog.SetDefault(l); _ = slog.LevelError; _ = slog.NewLogLogger(nil, slog.LevelError) }`, 0},
		{"fmt println", `func f() { fmt.Println("x") }`, 1},
		{"fmt printf", `func f() { fmt.Printf("x") }`, 1},
		{"fmt sprintf ok", `func f() { _ = fmt.Sprintf("x"); _ = fmt.Errorf("x") }`, 0},
		{"log default writer setters", `func f() { log.Default(); log.Writer(); log.SetOutput(nil); log.SetFlags(0); log.SetPrefix("") }`, 5},
		{"os newfile", `func f() { _ = os.NewFile(2, "x") }`, 1},
		{"injected logger", `func f(l *log.Logger) { l.Printf("x"); _ = log.New(nil, "", 0) }`, 0},
	}
	for _, tc := range cases {
		got, err := Violations("f.go", "package p\nimport (\"os\"; \"log\"; \"log/slog\"; \"fmt\"; \"context\")\n"+tc.body+"\n")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != tc.want {
			t.Errorf("%s: %d violations %v, want %d", tc.name, len(got), got, tc.want)
		}
	}
}

func TestViolationsResolvesImports(t *testing.T) {
	cases := []struct {
		name, src string
		want      int
	}{
		{"renamed log", "package p\nimport stdlog \"log\"\nfunc f() { stdlog.Printf(\"x\") }\n", 1},
		{"renamed os", "package p\nimport o \"os\"\nvar _ = o.Stderr\n", 1},
		{"renamed slog", "package p\nimport s \"log/slog\"\nfunc f() { s.Info(\"x\") }\n", 1},
		{"dot import log", "package p\nimport . \"log\"\nfunc f() { Printf(\"x\") }\n", 1},
		{"dot import fmt", "package p\nimport . \"fmt\"\n", 1},
		{"dot import other ok", "package p\nimport . \"strings\"\n", 0},
		{"syscall stderr", "package p\nimport \"syscall\"\nvar _ = syscall.Stderr\n", 1},
		{"syscall other ok", "package p\nimport \"syscall\"\nvar _ = syscall.SIGINT\n", 0},
		{"local var named log", "package p\nfunc f() { log := struct{ Printf func(string) }{}; log.Printf(\"x\") }\n", 0},
		{"params named os and slog", "package p\nfunc f(os struct{ Stderr int }, slog struct{ Info func() }) { _ = os.Stderr; slog.Info() }\n", 0},
		{"no import no flag", "package p\nfunc f(log interface{ Printf(string) }) { log.Printf(\"x\") }\n", 0},
		{"unrelated package named log", "package p\nimport log \"example.com/mylog\"\nfunc f() { log.Printf(\"x\") }\n", 0},
	}
	for _, tc := range cases {
		got, err := Violations("f.go", tc.src)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != tc.want {
			t.Errorf("%s: %d violations %v, want %d", tc.name, len(got), got, tc.want)
		}
	}
}
