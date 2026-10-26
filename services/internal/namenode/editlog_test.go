package namenode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sampleEntry(op EditOp, path string) EditEntry {
	return EditEntry{Op: op, Time: time.Date(2026, 3, 1, 15, 4, 5, 123456789, time.UTC), Path: path, Owner: "ana"}
}

func TestEditLogAppendYReabrir(t *testing.T) {
	dir := t.TempDir()
	l, entries, err := OpenEditLog(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("log nuevo: %v, %d entradas", err, len(entries))
	}
	if l.LastTxID() != 0 {
		t.Errorf("LastTxID de un log vacío = %d; quería 0", l.LastTxID())
	}
	for i, p := range []string{"/a", "/b", "/c"} {
		e := sampleEntry(OpMkdir, p)
		must(t, l.Append(&e))
		if e.TxID != int64(i+1) {
			t.Errorf("txid asignado = %d; quería %d", e.TxID, i+1)
		}
	}
	if l.LastTxID() != 3 || l.SinceCheckpoint() != 3 {
		t.Errorf("LastTxID=%d SinceCheckpoint=%d; quería 3 y 3", l.LastTxID(), l.SinceCheckpoint())
	}
	must(t, l.Close())

	l2, entries, err := OpenEditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if len(entries) != 3 || entries[0].Path != "/a" || entries[2].Path != "/c" || entries[2].TxID != 3 {
		t.Fatalf("entradas = %+v", entries)
	}
	if !entries[0].Time.Equal(sampleEntry(OpMkdir, "").Time) || entries[0].Owner != "ana" {
		t.Errorf("la hora y el dueño deben sobrevivir: %+v", entries[0])
	}
	// El siguiente txid continúa.
	e := sampleEntry(OpMkdir, "/d")
	must(t, l2.Append(&e))
	if e.TxID != 4 {
		t.Errorf("txid tras reabrir = %d; quería 4", e.TxID)
	}
}

func TestEditLogDescartaLaEscrituraIncompletaDelFinal(t *testing.T) {
	dir := t.TempDir()
	l, _, _ := OpenEditLog(dir)
	for _, p := range []string{"/a", "/b"} {
		e := sampleEntry(OpMkdir, p)
		must(t, l.Append(&e))
	}
	must(t, l.Close())

	path := filepath.Join(dir, editLogFileName)
	good, _ := os.ReadFile(path)
	// Una caída a mitad de escritura deja una línea sin terminar.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"txid":3,"op":"mkdir","pa`)
	f.Close()

	l2, entries, err := OpenEditLog(dir)
	if err != nil {
		t.Fatalf("una línea final incompleta no debe ser un error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entradas = %d; quería 2", len(entries))
	}
	// El archivo se reparó y se puede seguir escribiendo.
	e := sampleEntry(OpMkdir, "/c")
	must(t, l2.Append(&e))
	must(t, l2.Close())
	if e.TxID != 3 {
		t.Errorf("txid = %d; quería 3", e.TxID)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), string(good)) || strings.Contains(string(data), `"pa`+"\n") {
		t.Errorf("el log no quedó reparado: %q", data)
	}
	l3, entries, err := OpenEditLog(dir)
	if err == nil {
		defer l3.Close()
	}
	if err != nil || len(entries) != 3 {
		t.Errorf("tras reparar: %v, %d entradas", err, len(entries))
	}
}

func TestEditLogUltimaLineaCorruptaConSaltoDeLinea(t *testing.T) {
	dir := t.TempDir()
	l, _, _ := OpenEditLog(dir)
	e := sampleEntry(OpMkdir, "/a")
	must(t, l.Append(&e))
	must(t, l.Close())
	f, _ := os.OpenFile(filepath.Join(dir, editLogFileName), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("%%%basura%%%\n")
	f.Close()

	l3, entries, err := OpenEditLog(dir)
	if err == nil {
		defer l3.Close()
	}
	if err != nil || len(entries) != 1 {
		t.Errorf("la última línea corrupta se descarta: %v, %d entradas", err, len(entries))
	}
}

func TestEditLogCorruptoEnElMedioEsError(t *testing.T) {
	dir := t.TempDir()
	l, _, _ := OpenEditLog(dir)
	for _, p := range []string{"/a", "/b"} {
		e := sampleEntry(OpMkdir, p)
		must(t, l.Append(&e))
	}
	must(t, l.Close())
	path := filepath.Join(dir, editLogFileName)
	data, _ := os.ReadFile(path)
	lines := strings.SplitN(string(data), "\n", 2)
	must(t, os.WriteFile(path, []byte(lines[0]+"\n%%%basura%%%\n"+lines[1]), 0o600))

	if _, _, err := OpenEditLog(dir); err == nil || !strings.Contains(err.Error(), "corrupta") {
		t.Errorf("quería un error de línea corrupta, dio %v", err)
	}
}

func TestEditLogTxidNoConsecutivosEsError(t *testing.T) {
	dir := t.TempDir()
	content := `{"txid":1,"op":"mkdir","time":"2026-03-01T15:04:05Z","path":"/a"}` + "\n" +
		`{"txid":3,"op":"mkdir","time":"2026-03-01T15:04:05Z","path":"/b"}` + "\n"
	must(t, os.WriteFile(filepath.Join(dir, editLogFileName), []byte(content), 0o600))
	if _, _, err := OpenEditLog(dir); err == nil || !strings.Contains(err.Error(), "txid") {
		t.Errorf("quería un error de txid, dio %v", err)
	}
}

func TestEditLogResetConservaElTxid(t *testing.T) {
	dir := t.TempDir()
	l, _, _ := OpenEditLog(dir)
	for _, p := range []string{"/a", "/b"} {
		e := sampleEntry(OpMkdir, p)
		must(t, l.Append(&e))
	}
	must(t, l.Reset())
	if l.SinceCheckpoint() != 0 || l.LastTxID() != 2 {
		t.Errorf("tras Reset: Since=%d Last=%d; quería 0 y 2", l.SinceCheckpoint(), l.LastTxID())
	}
	if info, _ := os.Stat(filepath.Join(dir, editLogFileName)); info.Size() != 0 {
		t.Errorf("el archivo debía quedar vacío, tiene %d bytes", info.Size())
	}
	e := sampleEntry(OpMkdir, "/c")
	must(t, l.Append(&e))
	if e.TxID != 3 {
		t.Errorf("txid tras Reset = %d; quería 3", e.TxID)
	}
	must(t, l.Close())
	l3, entries, err := OpenEditLog(dir)
	if err == nil {
		defer l3.Close()
	}
	if err != nil || len(entries) != 1 || entries[0].TxID != 3 {
		t.Errorf("tras reabrir: %v, %+v", err, entries)
	}
}

func TestEditLogQuedaInutilizableSiFallaLaEscritura(t *testing.T) {
	l, _, _ := OpenEditLog(t.TempDir())
	e := sampleEntry(OpMkdir, "/a")
	must(t, l.Append(&e))
	must(t, l.f.Close()) // simula un disco que deja de responder

	e2 := sampleEntry(OpMkdir, "/b")
	if err := l.Append(&e2); err == nil {
		t.Fatal("Append debía fallar con el archivo cerrado")
	}
	e3 := sampleEntry(OpMkdir, "/c")
	if err := l.Append(&e3); err == nil || !strings.Contains(err.Error(), "no se puede usar") {
		t.Errorf("tras un fallo el log debe rechazar todo: %v", err)
	}
	if l.LastTxID() != 1 {
		t.Errorf("las entradas fallidas no deben consumir txid: %d", l.LastTxID())
	}
}

func TestEditLogOnAppend(t *testing.T) {
	l, _, _ := OpenEditLog(t.TempDir())
	defer l.Close()
	var seen []int64
	l.OnAppend = func(e EditEntry) error {
		seen = append(seen, e.TxID)
		return nil
	}
	for _, p := range []string{"/a", "/b"} {
		e := sampleEntry(OpMkdir, p)
		must(t, l.Append(&e))
	}
	if len(seen) != 2 || seen[0] != 1 || seen[1] != 2 {
		t.Errorf("OnAppend vio %v; quería [1 2]", seen)
	}
}
