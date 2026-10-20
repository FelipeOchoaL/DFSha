// Árbol de directorios y archivos (inodos): mkdir, rmdir, rm, ls, dueño y estado
// del archivo (UNDER_CONSTRUCTION / COMPLETE).
package namenode

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxPathLen es el largo máximo de una ruta del DFS (ver documentation/api-rest.md).
const maxPathLen = 4096

// Errores del namespace. La capa REST los traduce a códigos HTTP con errors.Is.
// Siempre se devuelven envueltos con la ruta que los causó.
var (
	ErrInvalidPath    = errors.New("ruta inválida")
	ErrNotFound       = errors.New("no existe")
	ErrParentNotFound = errors.New("el directorio padre no existe")
	ErrAlreadyExists  = errors.New("ya existe")
	ErrNotDirectory   = errors.New("no es un directorio")
	ErrIsDirectory    = errors.New("es un directorio")
	ErrNotEmpty       = errors.New("el directorio no está vacío")
)

// NodeType distingue directorios de archivos. Los valores coinciden con el
// campo "type" de la API REST.
type NodeType string

const (
	TypeDir  NodeType = "dir"
	TypeFile NodeType = "file"
)

// FileState es el estado de un archivo durante su escritura (write-once).
type FileState string

const (
	StateUnderConstruction FileState = "UNDER_CONSTRUCTION"
	StateComplete          FileState = "COMPLETE"
)

// Entry es una copia inmutable de los datos de un inodo. Se devuelve en lugar
// del inodo para que nadie lo modifique fuera del lock del Namespace.
type Entry struct {
	Name    string
	Type    NodeType
	Size    int64
	Owner   string
	ModTime time.Time
	// State solo aplica a archivos; vacío en directorios.
	State FileState
}

// inode es un nodo del árbol. Solo se toca con el lock del Namespace tomado.
type inode struct {
	name     string
	isDir    bool
	owner    string
	modTime  time.Time
	size     int64
	state    FileState
	children map[string]*inode // solo en directorios
}

func (n *inode) entry() Entry {
	e := Entry{
		Name:    n.name,
		Type:    TypeFile,
		Size:    n.size,
		Owner:   n.owner,
		ModTime: n.modTime,
	}
	if n.isDir {
		e.Type = TypeDir
		e.Size = 0
	} else {
		e.State = n.state
	}
	return e
}

// Namespace es el árbol de directorios del DFS en memoria. Es seguro para uso
// concurrente: las lecturas toman RLock y las mutaciones Lock.
type Namespace struct {
	mu   sync.RWMutex
	root *inode
	now  func() time.Time // inyectable para pruebas
}

// NewNamespace crea un namespace con solo el directorio raíz "/", de dueño "root".
func NewNamespace() *Namespace {
	ns := &Namespace{now: func() time.Time { return time.Now().UTC() }}
	ns.root = &inode{
		isDir:    true,
		owner:    "root",
		modTime:  ns.now(),
		children: make(map[string]*inode),
	}
	return ns
}

// NormalizePath valida y normaliza una ruta absoluta: "/a//b/../c" -> "/a/c".
// Devuelve ErrInvalidPath si está vacía, es relativa, es demasiado larga o
// contiene caracteres NUL.
func NormalizePath(p string) (string, error) {
	if p == "" || len(p) > maxPathLen || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, p)
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%w: debe ser absoluta: %q", ErrInvalidPath, p)
	}
	return path.Clean(p), nil // Clean("/..") == "/", no se puede salir de la raíz
}

// splitPath separa una ruta ya normalizada en sus componentes ("/" -> nil).
func splitPath(norm string) []string {
	if norm == "/" {
		return nil
	}
	return strings.Split(norm[1:], "/")
}

// find devuelve el inodo de una ruta normalizada. Debe llamarse con el lock tomado.
func (ns *Namespace) find(norm string) (*inode, error) {
	cur := ns.root
	for _, part := range splitPath(norm) {
		if !cur.isDir {
			return nil, fmt.Errorf("%w: %s", ErrNotDirectory, norm)
		}
		next, ok := cur.children[part]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, norm)
		}
		cur = next
	}
	return cur, nil
}

// parentOf devuelve el directorio padre de una ruta normalizada (distinta de "/")
// y el nombre del último componente. Falla con ErrParentNotFound si algún padre
// no existe y con ErrNotDirectory si alguno es un archivo.
func (ns *Namespace) parentOf(norm string) (*inode, string, error) {
	parts := splitPath(norm)
	name := parts[len(parts)-1]
	cur := ns.root
	for _, part := range parts[:len(parts)-1] {
		next, ok := cur.children[part]
		if !ok {
			return nil, "", fmt.Errorf("%w: %s", ErrParentNotFound, norm)
		}
		if !next.isDir {
			return nil, "", fmt.Errorf("%w: %s", ErrNotDirectory, norm)
		}
		cur = next
	}
	return cur, name, nil
}

// Mkdir crea un directorio. Con parents=true crea también los padres que falten
// y no falla si el directorio ya existe (como mkdir -p).
func (ns *Namespace) Mkdir(p, owner string, parents bool) error {
	norm, err := NormalizePath(p)
	if err != nil {
		return err
	}
	ns.mu.Lock()
	defer ns.mu.Unlock()

	if norm == "/" {
		if parents {
			return nil
		}
		return fmt.Errorf("%w: /", ErrAlreadyExists)
	}

	if !parents {
		parent, name, err := ns.parentOf(norm)
		if err != nil {
			return err
		}
		if _, exists := parent.children[name]; exists {
			return fmt.Errorf("%w: %s", ErrAlreadyExists, norm)
		}
		ns.addChild(parent, newDir(name, owner, ns.now()))
		return nil
	}

	// mkdir -p: avanza creando lo que falte. Se valida todo el recorrido antes de
	// crear nada para no dejar directorios a medias si un componente es un archivo.
	parts := splitPath(norm)
	cur := ns.root
	for i, part := range parts {
		next, ok := cur.children[part]
		if !ok {
			break
		}
		if !next.isDir {
			if i == len(parts)-1 {
				return fmt.Errorf("%w: %s", ErrAlreadyExists, norm)
			}
			return fmt.Errorf("%w: %s", ErrNotDirectory, norm)
		}
		cur = next
	}
	cur = ns.root
	for _, part := range parts {
		next, ok := cur.children[part]
		if !ok {
			next = newDir(part, owner, ns.now())
			ns.addChild(cur, next)
		}
		cur = next
	}
	return nil
}

// List devuelve el contenido de un directorio ordenado por nombre. Si la ruta es
// un archivo devuelve una sola entrada con ese archivo (como ls archivo).
func (ns *Namespace) List(p string) ([]Entry, error) {
	norm, err := NormalizePath(p)
	if err != nil {
		return nil, err
	}
	ns.mu.RLock()
	defer ns.mu.RUnlock()

	n, err := ns.find(norm)
	if err != nil {
		return nil, err
	}
	if !n.isDir {
		return []Entry{n.entry()}, nil
	}
	entries := make([]Entry, 0, len(n.children))
	for _, child := range n.children {
		entries = append(entries, child.entry())
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

// Stat devuelve los datos de un archivo o directorio. Para la raíz, Name es "".
func (ns *Namespace) Stat(p string) (Entry, error) {
	norm, err := NormalizePath(p)
	if err != nil {
		return Entry{}, err
	}
	ns.mu.RLock()
	defer ns.mu.RUnlock()

	n, err := ns.find(norm)
	if err != nil {
		return Entry{}, err
	}
	return n.entry(), nil
}

// CreateFile crea un archivo vacío en estado UNDER_CONSTRUCTION. Falla con
// ErrAlreadyExists si la ruta ya existe: es la operación atómica que usa
// allocate para que de dos subidas simultáneas al mismo path solo gane una.
func (ns *Namespace) CreateFile(p, owner string) error {
	norm, err := NormalizePath(p)
	if err != nil {
		return err
	}
	if norm == "/" {
		return fmt.Errorf("%w: no se puede crear un archivo en la raíz: %s", ErrInvalidPath, p)
	}
	ns.mu.Lock()
	defer ns.mu.Unlock()

	parent, name, err := ns.parentOf(norm)
	if err != nil {
		return err
	}
	if _, exists := parent.children[name]; exists {
		return fmt.Errorf("%w: %s", ErrAlreadyExists, norm)
	}
	ns.addChild(parent, &inode{
		name:    name,
		owner:   owner,
		modTime: ns.now(),
		state:   StateUnderConstruction,
	})
	return nil
}

// CompleteFile marca un archivo como COMPLETE y fija su tamaño total (finalize).
func (ns *Namespace) CompleteFile(p string, size int64) error {
	norm, err := NormalizePath(p)
	if err != nil {
		return err
	}
	ns.mu.Lock()
	defer ns.mu.Unlock()

	n, err := ns.find(norm)
	if err != nil {
		return err
	}
	if n.isDir {
		return fmt.Errorf("%w: %s", ErrIsDirectory, norm)
	}
	n.state = StateComplete
	n.size = size
	n.modTime = ns.now()
	return nil
}

// Rmdir elimina un directorio vacío. No permite borrar la raíz.
func (ns *Namespace) Rmdir(p string) error {
	norm, err := NormalizePath(p)
	if err != nil {
		return err
	}
	if norm == "/" {
		return fmt.Errorf("%w: no se puede eliminar la raíz", ErrInvalidPath)
	}
	ns.mu.Lock()
	defer ns.mu.Unlock()

	parent, name, err := ns.parentOf(norm)
	if err != nil {
		return notFoundIfParentMissing(err, norm)
	}
	n, ok := parent.children[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, norm)
	}
	if !n.isDir {
		return fmt.Errorf("%w: %s", ErrNotDirectory, norm)
	}
	if len(n.children) > 0 {
		return fmt.Errorf("%w: %s", ErrNotEmpty, norm)
	}
	ns.removeChild(parent, name)
	return nil
}

// Delete elimina un archivo (rm). Sobre un directorio devuelve ErrIsDirectory.
func (ns *Namespace) Delete(p string) error {
	norm, err := NormalizePath(p)
	if err != nil {
		return err
	}
	if norm == "/" {
		return fmt.Errorf("%w: no se puede eliminar la raíz", ErrInvalidPath)
	}
	ns.mu.Lock()
	defer ns.mu.Unlock()

	parent, name, err := ns.parentOf(norm)
	if err != nil {
		return notFoundIfParentMissing(err, norm)
	}
	n, ok := parent.children[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, norm)
	}
	if n.isDir {
		return fmt.Errorf("%w: %s", ErrIsDirectory, norm)
	}
	ns.removeChild(parent, name)
	return nil
}

// notFoundIfParentMissing: al borrar, que falte un padre significa que la ruta
// no existe (ErrNotFound), no que "falte el padre" como al crear.
func notFoundIfParentMissing(err error, norm string) error {
	if errors.Is(err, ErrParentNotFound) {
		return fmt.Errorf("%w: %s", ErrNotFound, norm)
	}
	return err
}

func newDir(name, owner string, t time.Time) *inode {
	return &inode{
		name:     name,
		isDir:    true,
		owner:    owner,
		modTime:  t,
		children: make(map[string]*inode),
	}
}

// addChild y removeChild actualizan también la fecha de modificación del padre.
func (ns *Namespace) addChild(parent, child *inode) {
	parent.children[child.name] = child
	parent.modTime = ns.now()
}

func (ns *Namespace) removeChild(parent *inode, name string) {
	delete(parent.children, name)
	parent.modTime = ns.now()
}
