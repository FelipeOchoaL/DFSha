package namenode

import (
"errors"
"fmt"
"sync"
"testing"
)

func TestNormalizePath(t *testing.T) {
ok := map[string]string{
"/":              "/",
"/a//b/../c":     "/a/c",
"/a/b/":          "/a/b",
"/a/./b":         "/a/b",
"/../a":          "/a",
"/user/ana/docs": "/user/ana/docs",
}
for in, want := range ok {
got, err := NormalizePath(in)
if err != nil || got != want {
t.Errorf("NormalizePath(%q) = %q, %v; quería %q", in, got, err, want)
}
}
for _, in := range []string{"", "a/b", "relativa", "/a\x00b"} {
if _, err := NormalizePath(in); !errors.Is(err, ErrInvalidPath) {
t.Errorf("NormalizePath(%q) debía fallar con ErrInvalidPath, dio %v", in, err)
}
}
long := "/" + string(make([]byte, 0)) + fmt.Sprintf("%04097d", 0)
if _, err := NormalizePath(long); !errors.Is(err, ErrInvalidPath) {
t.Errorf("una ruta de más de 4096 caracteres debía ser inválida, dio %v", err)
}
}

func TestMkdir(t *testing.T) {
ns := NewNamespace()

if err := ns.Mkdir("/user", "root", false); err != nil {
t.Fatalf("mkdir /user: %v", err)
}
if err := ns.Mkdir("/user/ana", "ana", false); err != nil {
t.Fatalf("mkdir /user/ana: %v", err)
}
e, err := ns.Stat("/user/ana")
if err != nil || e.Type != TypeDir || e.Owner != "ana" || e.Name != "ana" {
t.Errorf("Stat(/user/ana) = %+v, %v", e, err)
}

if err := ns.Mkdir("/user/ana", "ana", false); !errors.Is(err, ErrAlreadyExists) {
t.Errorf("mkdir repetido: quería ErrAlreadyExists, dio %v", err)
}
if err := ns.Mkdir("/x/y", "ana", false); !errors.Is(err, ErrParentNotFound) {
t.Errorf("mkdir sin padre: quería ErrParentNotFound, dio %v", err)
}
if err := ns.Mkdir("/", "ana", false); !errors.Is(err, ErrAlreadyExists) {
t.Errorf("mkdir /: quería ErrAlreadyExists, dio %v", err)
}
if err := ns.Mkdir("relativa", "ana", false); !errors.Is(err, ErrInvalidPath) {
t.Errorf("mkdir relativa: quería ErrInvalidPath, dio %v", err)
}
}

func TestMkdirParents(t *testing.T) {
ns := NewNamespace()

if err := ns.Mkdir("/a/b/c", "ana", true); err != nil {
t.Fatalf("mkdir -p: %v", err)
}
for _, p := range []string{"/a", "/a/b", "/a/b/c"} {
if e, err := ns.Stat(p); err != nil || e.Type != TypeDir {
t.Errorf("%s debía ser un directorio: %+v, %v", p, e, err)
}
}
// Repetir con parents=true no falla.
if err := ns.Mkdir("/a/b/c", "ana", true); err != nil {
t.Errorf("mkdir -p repetido no debía fallar: %v", err)
}
if err := ns.Mkdir("/", "ana", true); err != nil {
t.Errorf("mkdir -p / no debía fallar: %v", err)
}

// Un archivo en el camino.
if err := ns.CreateFile("/a/f.txt", "ana"); err != nil {
t.Fatal(err)
}
if err := ns.Mkdir("/a/f.txt", "ana", true); !errors.Is(err, ErrAlreadyExists) {
t.Errorf("mkdir -p sobre un archivo: quería ErrAlreadyExists, dio %v", err)
}
if err := ns.Mkdir("/a/f.txt/z/w", "ana", true); !errors.Is(err, ErrNotDirectory) {
t.Errorf("mkdir -p a través de un archivo: quería ErrNotDirectory, dio %v", err)
}
// No debe haber dejado nada creado a medias.
if _, err := ns.Stat("/a/f.txt/z"); err == nil {
t.Error("no debía existir /a/f.txt/z")
}
if err := ns.Mkdir("/a/f.txt/z", "ana", false); !errors.Is(err, ErrNotDirectory) {
t.Errorf("mkdir dentro de un archivo: quería ErrNotDirectory, dio %v", err)
}
}

func TestList(t *testing.T) {
ns := NewNamespace()
must(t, ns.Mkdir("/d", "ana", false))
must(t, ns.Mkdir("/b", "ana", false))
must(t, ns.CreateFile("/c.txt", "ana"))
must(t, ns.CompleteFile("/c.txt", 42))

entries, err := ns.List("/")
if err != nil {
t.Fatal(err)
}
if len(entries) != 3 {
t.Fatalf("quería 3 entradas, hay %d: %+v", len(entries), entries)
}
names := []string{entries[0].Name, entries[1].Name, entries[2].Name}
if names[0] != "b" || names[1] != "c.txt" || names[2] != "d" {
t.Errorf("entradas sin ordenar por nombre: %v", names)
}
if entries[1].Type != TypeFile || entries[1].Size != 42 || entries[1].State != StateComplete {
t.Errorf("entrada de archivo incorrecta: %+v", entries[1])
}
if entries[0].Type != TypeDir || entries[0].State != "" || entries[0].Size != 0 {
t.Errorf("entrada de directorio incorrecta: %+v", entries[0])
}

// ls sobre un archivo devuelve solo ese archivo.
one, err := ns.List("/c.txt")
if err != nil || len(one) != 1 || one[0].Name != "c.txt" {
t.Errorf("List(archivo) = %+v, %v", one, err)
}
// Directorio vacío.
empty, err := ns.List("/b")
if err != nil || len(empty) != 0 {
t.Errorf("List(dir vacío) = %+v, %v", empty, err)
}
if _, err := ns.List("/no-existe"); !errors.Is(err, ErrNotFound) {
t.Errorf("List inexistente: quería ErrNotFound, dio %v", err)
}
if _, err := ns.List("/c.txt/x"); !errors.Is(err, ErrNotDirectory) {
t.Errorf("List a través de un archivo: quería ErrNotDirectory, dio %v", err)
}
}

func TestCreateFile(t *testing.T) {
ns := NewNamespace()
must(t, ns.Mkdir("/user", "root", false))

must(t, ns.CreateFile("/user/a.txt", "ana"))
e, err := ns.Stat("/user/a.txt")
if err != nil || e.Type != TypeFile || e.State != StateUnderConstruction || e.Owner != "ana" || e.Size != 0 {
t.Errorf("archivo recién creado incorrecto: %+v, %v", e, err)
}

if err := ns.CreateFile("/user/a.txt", "bob"); !errors.Is(err, ErrAlreadyExists) {
t.Errorf("crear archivo repetido: quería ErrAlreadyExists, dio %v", err)
}
if err := ns.CreateFile("/user", "bob"); !errors.Is(err, ErrAlreadyExists) {
t.Errorf("crear archivo sobre un directorio: quería ErrAlreadyExists, dio %v", err)
}
if err := ns.CreateFile("/nope/a.txt", "ana"); !errors.Is(err, ErrParentNotFound) {
t.Errorf("crear sin padre: quería ErrParentNotFound, dio %v", err)
}
if err := ns.CreateFile("/user/a.txt/b", "ana"); !errors.Is(err, ErrNotDirectory) {
t.Errorf("crear dentro de un archivo: quería ErrNotDirectory, dio %v", err)
}
if err := ns.CreateFile("/", "ana"); !errors.Is(err, ErrInvalidPath) {
t.Errorf("crear archivo en la raíz: quería ErrInvalidPath, dio %v", err)
}

// CompleteFile
must(t, ns.CompleteFile("/user/a.txt", 1234))
e, _ = ns.Stat("/user/a.txt")
if e.State != StateComplete || e.Size != 1234 {
t.Errorf("después de CompleteFile: %+v", e)
}
if err := ns.CompleteFile("/user", 1); !errors.Is(err, ErrIsDirectory) {
t.Errorf("CompleteFile sobre un directorio: quería ErrIsDirectory, dio %v", err)
}
if err := ns.CompleteFile("/user/zzz", 1); !errors.Is(err, ErrNotFound) {
t.Errorf("CompleteFile inexistente: quería ErrNotFound, dio %v", err)
}
}

func TestRmdir(t *testing.T) {
ns := NewNamespace()
must(t, ns.Mkdir("/a/b", "ana", true))
must(t, ns.CreateFile("/a/f.txt", "ana"))

if err := ns.Rmdir("/a"); !errors.Is(err, ErrNotEmpty) {
t.Errorf("rmdir de un directorio con contenido: quería ErrNotEmpty, dio %v", err)
}
if err := ns.Rmdir("/a/f.txt"); !errors.Is(err, ErrNotDirectory) {
t.Errorf("rmdir de un archivo: quería ErrNotDirectory, dio %v", err)
}
if err := ns.Rmdir("/zzz"); !errors.Is(err, ErrNotFound) {
t.Errorf("rmdir inexistente: quería ErrNotFound, dio %v", err)
}
if err := ns.Rmdir("/zzz/yyy"); !errors.Is(err, ErrNotFound) {
t.Errorf("rmdir con padre inexistente: quería ErrNotFound, dio %v", err)
}
if err := ns.Rmdir("/"); !errors.Is(err, ErrInvalidPath) {
t.Errorf("rmdir /: quería ErrInvalidPath, dio %v", err)
}

must(t, ns.Rmdir("/a/b"))
if _, err := ns.Stat("/a/b"); !errors.Is(err, ErrNotFound) {
t.Errorf("/a/b debía haber desaparecido, dio %v", err)
}
}

func TestDelete(t *testing.T) {
ns := NewNamespace()
must(t, ns.Mkdir("/a", "ana", false))
must(t, ns.CreateFile("/a/f.txt", "ana"))

if err := ns.Delete("/a"); !errors.Is(err, ErrIsDirectory) {
t.Errorf("rm de un directorio: quería ErrIsDirectory, dio %v", err)
}
if err := ns.Delete("/a/zzz"); !errors.Is(err, ErrNotFound) {
t.Errorf("rm inexistente: quería ErrNotFound, dio %v", err)
}
if err := ns.Delete("/q/zzz"); !errors.Is(err, ErrNotFound) {
t.Errorf("rm con padre inexistente: quería ErrNotFound, dio %v", err)
}
if err := ns.Delete("/"); !errors.Is(err, ErrInvalidPath) {
t.Errorf("rm /: quería ErrInvalidPath, dio %v", err)
}

must(t, ns.Delete("/a/f.txt"))
if _, err := ns.Stat("/a/f.txt"); !errors.Is(err, ErrNotFound) {
t.Errorf("/a/f.txt debía haber desaparecido, dio %v", err)
}
// Ahora el directorio está vacío y se puede borrar.
must(t, ns.Rmdir("/a"))
}

func TestRutasNormalizadasEnOperaciones(t *testing.T) {
ns := NewNamespace()
must(t, ns.Mkdir("/a//b/../c", "ana", true))
if _, err := ns.Stat("/a/c"); err != nil {
t.Errorf("/a/c debía existir: %v", err)
}
if _, err := ns.Stat("/a/b"); err == nil {
t.Error("/a/b no debía existir")
}
}

func TestListDevuelveCopias(t *testing.T) {
ns := NewNamespace()
must(t, ns.CreateFile("/f", "ana"))
e, _ := ns.List("/")
e[0].Owner = "intruso"
again, _ := ns.Stat("/f")
if again.Owner != "ana" {
t.Error("modificar la Entry devuelta no debe afectar al namespace")
}
}

// Dos subidas simultáneas al mismo path: solo una crea el archivo.
func TestCreateFileConcurrente(t *testing.T) {
ns := NewNamespace()
const n = 50
var wg sync.WaitGroup
results := make(chan error, n)
for i := 0; i < n; i++ {
wg.Add(1)
go func(i int) {
defer wg.Done()
results <- ns.CreateFile("/mismo.txt", fmt.Sprintf("u%d", i))
}(i)
}
wg.Wait()
close(results)

ok := 0
for err := range results {
if err == nil {
ok++
} else if !errors.Is(err, ErrAlreadyExists) {
t.Errorf("error inesperado: %v", err)
}
}
if ok != 1 {
t.Errorf("quería exactamente 1 creación exitosa, hubo %d", ok)
}
}

// Operaciones mezcladas desde varias goroutines (útil con go test -race).
func TestConcurrenciaMixta(t *testing.T) {
ns := NewNamespace()
var wg sync.WaitGroup
for i := 0; i < 20; i++ {
wg.Add(1)
go func(i int) {
defer wg.Done()
dir := fmt.Sprintf("/d%d", i)
_ = ns.Mkdir(dir, "ana", false)
_ = ns.CreateFile(dir+"/f", "ana")
_, _ = ns.List("/")
_, _ = ns.List(dir)
_ = ns.Delete(dir + "/f")
_ = ns.Rmdir(dir)
}(i)
}
wg.Wait()
entries, err := ns.List("/")
if err != nil || len(entries) != 0 {
t.Errorf("al final no debía quedar nada: %+v, %v", entries, err)
}
}

func must(t *testing.T, err error) {
t.Helper()
if err != nil {
t.Fatal(err)
}
}