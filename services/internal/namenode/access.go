// Control de acceso por usuario y grupo (US-82): permisos rwx estilo Linux.
//
// Cada inodo tiene dueño, grupo y permisos para el dueño, el grupo y los demás.
// Como en Linux, para llegar a un archivo hace falta permiso de ejecución (x) en
// todos los directorios del camino; leer un directorio pide r; crear o borrar una
// entrada pide w y x en el directorio que la contiene. El superusuario (usuario
// "root" o grupo "admin") puede todo.
package namenode

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Permisos por defecto de lo que se crea: el dueño todo, el grupo leer, los demás
// nada. Así los archivos son privados salvo que el dueño los abra con chmod.
const (
	defaultDirMode  uint32 = 0o750
	defaultFileMode uint32 = 0o640
	// Directorio /user: todos pueden recorrerlo y listarlo; solo root escribe.
	homeParentMode uint32 = 0o755
)

// Nombres con privilegios de superusuario.
const (
	superUser  = "root"
	superGroup = "admin"
)

// Access son los permisos que pide una operación (combinables con |).
type Access uint8

const (
	AccessExec  Access = 1
	AccessWrite Access = 2
	AccessRead  Access = 4
)

// ErrPermissionDenied: el usuario no tiene permiso. La API lo traduce a 403 FORBIDDEN.
var ErrPermissionDenied = errors.New("permiso denegado")

// Principal es el usuario que hace una petición, con sus grupos (del JWT, US-80).
type Principal struct {
	User   string
	Groups []string
	// Super marca a un usuario con todos los permisos. También lo son el usuario
	// "root" y los miembros del grupo "admin".
	Super bool
}

// PrimaryGroup es el grupo con el que se crean los archivos del usuario: el primero de su lista.
func (p Principal) PrimaryGroup() string {
	if len(p.Groups) == 0 {
		return ""
	}
	return p.Groups[0]
}

func (p Principal) inGroup(g string) bool {
	if g == "" {
		return false
	}
	for _, x := range p.Groups {
		if x == g {
			return true
		}
	}
	return false
}

// isSuper indica si p tiene privilegios de superusuario.
func (p Principal) isSuper() bool {
	return p.Super || p.User == superUser || p.inGroup(superGroup)
}

// AccessReq describe los permisos que necesita una operación sobre una ruta.
type AccessReq struct {
	// Parent: permisos sobre el directorio que contiene la ruta (w|x para crear o borrar).
	Parent Access
	// Dir y File: permisos sobre la ruta misma, según sea directorio o archivo.
	Dir, File Access
	// CreateParents: la operación crea los directorios intermedios que falten, así
	// que se piden los permisos de Parent sobre el más profundo que ya exista.
	CreateParents bool
}

// canAccess indica si who tiene todos los permisos de need sobre n.
func canAccess(n *inode, who Principal, need Access) bool {
	if need == 0 || who.Super {
		return true
	}
	var bits uint32
	switch {
	case who.User == n.owner:
		bits = (n.mode >> 6) & 7
	case who.inGroup(n.group):
		bits = (n.mode >> 3) & 7
	default:
		bits = n.mode & 7
	}
	return bits&uint32(need) == uint32(need)
}

// Authorize comprueba que who puede hacer sobre la ruta p lo que describe req.
// Devuelve ErrPermissionDenied si no. Si la ruta o algún directorio del camino no
// existe no falla (la operación devolverá su error normal), salvo que ya falte
// permiso para recorrer los directorios que sí existen: así no se revela si algo
// existe a quien no puede ver el directorio que lo contiene.
func (ns *Namespace) Authorize(p string, who Principal, req AccessReq) error {
	norm, err := NormalizePath(p)
	if err != nil {
		return err
	}
	ns.mu.RLock()
	defer ns.mu.RUnlock()

	denied := fmt.Errorf("%w: %s", ErrPermissionDenied, norm)
	parts := splitPath(norm)
	if len(parts) == 0 {
		if !canAccess(ns.root, who, req.Dir) {
			return denied
		}
		return nil
	}
	cur := ns.root
	for i, part := range parts {
		last := i == len(parts)-1
		if !canAccess(cur, who, AccessExec) { // para buscar `part` dentro de cur
			return denied
		}
		next, ok := cur.children[part]
		if !ok {
			if (last || req.CreateParents) && !canAccess(cur, who, req.Parent) {
				return denied
			}
			return nil
		}
		if last {
			if !canAccess(cur, who, req.Parent) {
				return denied
			}
			need := req.File
			if next.isDir {
				need = req.Dir
			}
			if !canAccess(next, who, need) {
				return denied
			}
			return nil
		}
		if !next.isDir {
			return nil // la operación responderá NOT_A_DIRECTORY
		}
		cur = next
	}
	return nil
}

// SetMode cambia los permisos de un archivo o directorio.
func (ns *Namespace) SetMode(p string, mode uint32) error {
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
	n.mode = mode & 0o777
	return nil
}

// ParseMode lee permisos en octal ("640" o "0640").
func ParseMode(s string) (uint32, error) {
	s = strings.TrimSpace(s)
	if len(s) < 3 || len(s) > 4 {
		return 0, fmt.Errorf("permisos inválidos %q: use octal, por ejemplo 0640", s)
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil || v > 0o777 {
		return 0, fmt.Errorf("permisos inválidos %q: use octal entre 000 y 0777", s)
	}
	return uint32(v), nil
}

// FormatMode devuelve los permisos como "rw-r-----".
func FormatMode(mode uint32) string {
	const rwx = "rwxrwxrwx"
	b := []byte("---------")
	for i := 0; i < 9; i++ {
		if mode&(1<<uint(8-i)) != 0 {
			b[i] = rwx[i]
		}
	}
	return string(b)
}

func modeOr(mode, fallback uint32) uint32 {
	if mode == 0 {
		return fallback
	}
	return mode
}

// --- identidad de la petición ---

type principalKey struct{}

// principalOf devuelve el usuario autenticado de la petición (lo fija route).
func principalOf(r *http.Request) Principal {
	p, _ := r.Context().Value(principalKey{}).(Principal)
	return p
}

// identify obtiene el usuario de la petición: Identify si está configurado, si no
// Authenticate (sin grupos) y, si tampoco, un usuario anónimo con todos los
// permisos (modo sin autenticación, solo para desarrollo).
func (s *RESTServer) identify(r *http.Request) (Principal, error) {
	switch {
	case s.Identify != nil:
		return s.Identify(r)
	case s.Authenticate != nil:
		user, err := s.Authenticate(r)
		return Principal{User: user}, err
	default:
		return Principal{User: anonymousUser, Super: true}, nil
	}
}

func withPrincipal(r *http.Request, p Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
}

// --- primer login: directorio personal ---

// EnsureHome crea /user (si no existe) y /user/<usuario> (si no existe) para el
// usuario que acaba de iniciar sesión. Lo llama el login (US-80) tras validar la
// contraseña. El directorio personal es del usuario, con su grupo principal y
// permisos 0750. Es idempotente y registra los cambios en el edit log.
func (s *RESTServer) EnsureHome(who Principal) error {
	if who.User == "" || strings.ContainsAny(who.User, "/\x00") || who.User == "." || who.User == ".." {
		return fmt.Errorf("nombre de usuario inválido para crear su directorio: %q", who.User)
	}
	s.up.mu.Lock()
	defer s.up.mu.Unlock()

	if _, err := s.ns.Stat("/user"); err != nil {
		e := EditEntry{Op: OpMkdir, Time: s.stamp(), Path: "/user", Owner: superUser, Mode: homeParentMode}
		if err := s.applyMkdir(e); err != nil {
			return err
		}
		if err := s.journal(&e); err != nil {
			return err
		}
	}
	home := "/user/" + who.User
	if _, err := s.ns.Stat(home); err == nil {
		return nil
	}
	e := EditEntry{Op: OpMkdir, Time: s.stamp(), Path: home, Owner: who.User, Group: who.PrimaryGroup(), Mode: defaultDirMode}
	if err := s.applyMkdir(e); err != nil {
		return err
	}
	return s.journal(&e)
}

// --- POST /fs/chmod ---

type chmodRequest struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
}

type chmodResponse struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
}

// handleChmod cambia los permisos de un archivo o directorio. Solo su dueño o el
// superusuario.
func (s *RESTServer) handleChmod(w http.ResponseWriter, r *http.Request, _ string) {
	var req chmodRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Path == "" || req.Mode == "" {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "faltan campos obligatorios: path, mode")
		return
	}
	mode, err := ParseMode(req.Mode)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	norm, err := NormalizePath(req.Path)
	if err != nil {
		writeNamespaceError(w, err)
		return
	}
	who := principalOf(r)

	s.up.mu.Lock()
	defer s.up.mu.Unlock()

	if err := s.ns.Authorize(norm, who, AccessReq{}); err != nil {
		writeNamespaceError(w, err)
		return
	}
	st, err := s.ns.Stat(norm)
	if err != nil {
		writeNamespaceError(w, err)
		return
	}
	if st.Owner != who.User && !who.Super {
		writeNamespaceError(w, fmt.Errorf("%w: solo el dueño puede cambiar los permisos de %s", ErrPermissionDenied, norm))
		return
	}
	entry := EditEntry{Op: OpChmod, Time: s.stamp(), Path: norm, Mode: mode}
	if err := s.applyChmod(entry); err != nil {
		writeNamespaceError(w, err)
		return
	}
	if !s.record(w, &entry) {
		return
	}
	writeJSON(w, http.StatusOK, chmodResponse{Path: norm, Mode: fmt.Sprintf("%04o", mode)})
}
