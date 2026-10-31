package botsandbox

import (
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestEnsureImageTagReturnsBuildingImmediately 是这次改动的核心不变量：
// 镜像不存在时必须立刻返回 ErrImageBuilding，而不是同步等 docker build。
// 首次部署要把构建（数分钟）挡在 fx 启动超时的射程之外，靠的就是这一点。
func TestEnsureImageTagReturnsBuildingImmediately(t *testing.T) {
	resetBuildsForTest()
	tag := "thinkbot-bot:test-async-notblocking"

	release := make(chan struct{})
	buildHook = func(b *imageBuild, l *zap.SugaredLogger) error {
		<-release
		return nil
	}
	defer func() { buildHook = nil }()

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := ensureImageTag(tag, zap.NewNop().Sugar())
		done <- err
	}()

	select {
	case err := <-done:
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("ensureImageTag blocked for %s", elapsed)
		}
		if err == nil {
			t.Fatal("expected an error while the image is missing")
		}
		if !errors.Is(err, ErrImageBuilding) {
			t.Fatalf("expected ErrImageBuilding, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ensureImageTag did not return; it is still blocking on the build")
	}

	// 同一 tag 的并发调用必须共享一次构建，不能各起一个 docker build。
	buildsMu.Lock()
	_, ok := builds[tag]
	buildsMu.Unlock()
	if !ok {
		t.Fatal("background build was not registered")
	}

	close(release)
}

func TestBuildLineWriterSplitsLines(t *testing.T) {
	var got []string
	w := &buildLineWriter{onLine: func(line string) { got = append(got, line) }}

	if _, err := w.Write([]byte("Step 1/3 : FROM x\nStep 2/3 : RUN y\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("Step 3/3 : ENTRYPOINT")); err != nil {
		t.Fatal(err)
	}
	w.flush()

	want := []string{"Step 1/3 : FROM x", "Step 2/3 : RUN y", "Step 3/3 : ENTRYPOINT"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuildLineWriterHandlesCRLF(t *testing.T) {
	var got []string
	w := &buildLineWriter{onLine: func(line string) { got = append(got, line) }}
	if _, err := w.Write([]byte("Successfully built abc\r\n")); err != nil {
		t.Fatal(err)
	}
	w.flush()
	if len(got) != 1 || got[0] != "Successfully built abc" {
		t.Fatalf("got %q", got)
	}
}
