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
	Group   string
	Mode    uint32 // permisos rwx (0o750...)
	ModTime time.Time
	// State solo aplica a archivos; vacío en directorios.
	State FileState
}

// inode es un nodo del árbol. Solo se toca con el lock del Namespace tomado.
type inode struct {
	name     string
	isDir    bool
	owner    string
	group    string
	mode     uint32
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
		Group:   n.group,
		Mode:    n.mode,
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
		mode:     0o755,
		modTime:  time.Unix(0, 0).UTC(), // fija: la raíz no cambia entre reinicios sin hijos
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
	return ns.MkdirAs(p, owner, "", defaultDirMode, parents)
}

// MkdirAs es Mkdir con grupo y permisos explicitos para los directorios que cree.
func (ns *Namespace) MkdirAs(p, owner, group string, mode uint32, parents bool) error {
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
		ns.addChild(parent, newDir(name, owner, group, mode, ns.now()))
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
			next = newDir(part, owner, group, mode, ns.now())
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
	return ns.CreateFileAs(p, owner, "", defaultFileMode)
}

// CreateFileAs es CreateFile con grupo y permisos explicitos.
func (ns *Namespace) CreateFileAs(p, owner, group string, mode uint32) error {
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
		group:   group,
		mode:    mode,
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

func newDir(name, owner, group string, mode uint32, t time.Time) *inode {
	return &inode{
		name:     name,
		isDir:    true,
		owner:    owner,
		group:    group,
		mode:     mode,
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

// --- soporte para persistencia (edit log y snapshot, US-70) ---

// CanCreateFile informa si CreateFile(p, ...) tendría éxito, sin crear nada:
// devuelve los mismos errores. Permite validar antes de hacer otros pasos
// (como elegir DataNodes) sin dejar cambios a medias.
func (ns *Namespace) CanCreateFile(p string) error {
	norm, err := NormalizePath(p)
	if err != nil {
		return err
	}
	if norm == "/" {
		return fmt.Errorf("%w: no se puede crear un archivo en la raíz: %s", ErrInvalidPath, p)
	}
	ns.mu.RLock()
	defer ns.mu.RUnlock()

	parent, name, err := ns.parentOf(norm)
	if err != nil {
		return err
	}
	if _, exists := parent.children[name]; exists {
		return fmt.Errorf("%w: %s", ErrAlreadyExists, norm)
	}
	return nil
}

// setNow fija la hora que usarán las siguientes mutaciones. Se usa para que una
// operación tenga exactamente la misma fecha cuando se ejecuta en vivo y cuando
// se reproduce desde el edit log. Las mutaciones se serializan fuera de aquí.
func (ns *Namespace) setNow(t time.Time) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	ns.now = func() time.Time { return t }
}

// snapInode es la forma serializable de un inodo en el snapshot.
type snapInode struct {
	Name     string       `json:"name"`
	Dir      bool         `json:"dir,omitempty"`
	Owner    string       `json:"owner"`
	Group    string       `json:"group,omitempty"`
	Mode     *uint32      `json:"mode,omitempty"` // nil en snapshots antiguos: se usan los permisos por defecto
	ModTime  time.Time    `json:"modTime"`
	Size     int64        `json:"size,omitempty"`
	State    FileState    `json:"state,omitempty"`
	Children []*snapInode `json:"children,omitempty"`
}

// export devuelve una copia serializable de todo el árbol (hijos ordenados por nombre).
func (ns *Namespace) export() *snapInode {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	return exportInode(ns.root)
}

func exportInode(n *inode) *snapInode {
	mode := n.mode
	s := &snapInode{
		Name:    n.name,
		Dir:     n.isDir,
		Owner:   n.owner,
		Group:   n.group,
		Mode:    &mode,
		ModTime: n.modTime,
		Size:    n.size,
		State:   n.state,
	}
	if n.isDir {
		names := make([]string, 0, len(n.children))
		for name := range n.children {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			s.Children = append(s.Children, exportInode(n.children[name]))
		}
	}
	return s
}

// restore reemplaza todo el árbol por el del snapshot.
func (ns *Namespace) restore(root *snapInode) error {
	if root == nil || !root.Dir {
		return fmt.Errorf("snapshot inválido: la raíz debe ser un directorio")
	}
	rebuilt, err := restoreInode(root, "/")
	if err != nil {
		return err
	}
	ns.mu.Lock()
	defer ns.mu.Unlock()
	ns.root = rebuilt
	return nil
}

func restoreInode(s *snapInode, where string) (*inode, error) {
	n := &inode{
		name:    s.Name,
		isDir:   s.Dir,
		owner:   s.Owner,
		group:   s.Group,
		modTime: s.ModTime,
		size:    s.Size,
		state:   s.State,
	}
	if s.Mode != nil {
		n.mode = *s.Mode
	} else if where == "/" {
		n.mode = 0o755 // raiz de un snapshot antiguo
	} else if s.Dir {
		n.mode = defaultDirMode
	} else {
		n.mode = defaultFileMode
	}
	if !s.Dir {
		if len(s.Children) > 0 {
			return nil, fmt.Errorf("snapshot inválido: el archivo %s tiene hijos", where)
		}
		return n, nil
	}
	n.children = make(map[string]*inode, len(s.Children))
	for _, c := range s.Children {
		if c.Name == "" || strings.Contains(c.Name, "/") {
			return nil, fmt.Errorf("snapshot inválido: nombre %q en %s", c.Name, where)
		}
		if _, dup := n.children[c.Name]; dup {
			return nil, fmt.Errorf("snapshot inválido: %q repetido en %s", c.Name, where)
		}
		child, err := restoreInode(c, path.Join(where, c.Name))
		if err != nil {
			return nil, err
		}
		n.children[c.Name] = child
	}
	return n, nil
}
