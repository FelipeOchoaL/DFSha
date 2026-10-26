// Persistencia de los metadatos: aplicar operaciones (en vivo y al reproducir el
// edit log), registrarlas en disco, guardar snapshots y recuperar el estado.
//
// Todas las mutaciones de metadatos (mkdir, rmdir, rm, allocate, commit,
// finalize) se serializan con up.mu: dentro de ese lock se aplican en memoria y
// se escriben en el edit log, así que el orden del log es exactamente el orden
// en que se aplicaron y reproducirlo da el mismo estado.
package namenode

import (
	"fmt"
	"log"
	"net/http"
	"time"
)

// stamp devuelve la hora que se registra en una operación.
func (s *RESTServer) stamp() time.Time {
	// Se trunca a milisegundos: es la precision del proto (timestamp_unix_ms), asi la hora
	// es identica en el activo, en el edit log y en el standby. Truncate tambien quita el
	// reloj monotonico, para que la hora sobreviva al JSON.
	return time.Now().UTC().Truncate(time.Millisecond)
}

// --- aplicar operaciones (con up.mu tomado) ---
//
// Los apply* hacen el cambio de estado de una operación ya validada. Los usan
// los handlers REST y la reproducción del log (applyEntry), de modo que ambos
// producen el mismo resultado.

func (s *RESTServer) applyMkdir(e EditEntry) error {
	s.ns.setNow(e.Time)
	return s.ns.MkdirAs(e.Path, e.Owner, e.Group, modeOr(e.Mode, defaultDirMode), e.Parents)
}

func (s *RESTServer) applyRmdir(e EditEntry) error {
	s.ns.setNow(e.Time)
	return s.ns.Rmdir(e.Path)
}

// applyDelete elimina un archivo, cierra su subida si estaba en curso y devuelve
// los bloques que tenía (con las réplicas conocidas) para ordenar su borrado.
func (s *RESTServer) applyDelete(e EditEntry) ([]BlockInfo, error) {
	s.ns.setNow(e.Time)
	if err := s.ns.Delete(e.Path); err != nil {
		return nil, err
	}
	delete(s.up.byPath, e.Path)
	return s.blocks.RemoveFile(e.Path), nil
}

// applyAllocate abre la subida (si e.NewUpload crea el archivo) y asigna el
// bloque e.BlockID como pendiente, descartando el pendiente anterior si lo hay.
// Un allocate sin BlockID solo abre la subida de un archivo vacío.
func (s *RESTServer) applyAllocate(e EditEntry) error {
	var u *upload
	if e.NewUpload {
		if _, exists := s.up.byPath[e.Path]; exists {
			return fmt.Errorf("allocate: ya hay una subida de %s", e.Path)
		}
		s.ns.setNow(e.Time)
		if err := s.ns.CreateFileAs(e.Path, e.Owner, e.Group, modeOr(e.Mode, defaultFileMode)); err != nil {
			return err
		}
		u = &upload{id: e.UploadID, owner: e.Owner, attempts: make(map[int]int)}
		s.up.byPath[e.Path] = u
	} else {
		u = s.up.byPath[e.Path]
		if u == nil || u.id != e.UploadID {
			return fmt.Errorf("allocate: no hay una subida %s de %s", e.UploadID, e.Path)
		}
	}
	if e.BlockID == "" {
		return nil
	}
	if u.pending != nil {
		s.blocks.RemoveBlock(e.Path, u.pending.id)
		u.pending = nil
	}
	if err := s.blocks.AddFileBlock(e.Path, e.BlockID, e.Size); err != nil {
		return err
	}
	u.attempts[e.BlockIndex]++
	u.pending = &pendingBlock{id: e.BlockID, index: e.BlockIndex, size: e.Size}
	return nil
}

// applyCommit confirma el bloque pendiente de la subida.
func (s *RESTServer) applyCommit(e EditEntry) error {
	u := s.up.byPath[e.Path]
	if u == nil || u.id != e.UploadID {
		return fmt.Errorf("commit: no hay una subida %s de %s", e.UploadID, e.Path)
	}
	if u.pending == nil || u.pending.id != e.BlockID {
		return fmt.Errorf("commit: el bloque %s no está pendiente en %s", e.BlockID, e.Path)
	}
	if err := s.blocks.CommitBlock(e.BlockID, e.Size, e.Checksum); err != nil {
		return err
	}
	u.committed = append(u.committed, committedBlock{id: e.BlockID, size: e.Size, checksum: e.Checksum})
	u.committedSize += e.Size
	u.pending = nil
	return nil
}

// applyFinalize cierra el archivo (COMPLETE) y termina la subida.
func (s *RESTServer) applyFinalize(e EditEntry) error {
	u := s.up.byPath[e.Path]
	if u == nil || u.id != e.UploadID {
		return fmt.Errorf("finalize: no hay una subida %s de %s", e.UploadID, e.Path)
	}
	s.ns.setNow(e.Time)
	if err := s.ns.CompleteFile(e.Path, u.committedSize); err != nil {
		return err
	}
	delete(s.up.byPath, e.Path)
	return nil
}

// applyChmod cambia los permisos de un archivo o directorio.
func (s *RESTServer) applyChmod(e EditEntry) error {
	return s.ns.SetMode(e.Path, e.Mode)
}

// applyEntry aplica una entrada del edit log. Con up.mu tomado.
func (s *RESTServer) applyEntry(e EditEntry) error {
	var err error
	switch e.Op {
	case OpMkdir:
		err = s.applyMkdir(e)
	case OpRmdir:
		err = s.applyRmdir(e)
	case OpDelete:
		_, err = s.applyDelete(e)
	case OpAllocate:
		err = s.applyAllocate(e)
	case OpCommit:
		err = s.applyCommit(e)
	case OpChmod:
		err = s.applyChmod(e)
	case OpFinalize:
		err = s.applyFinalize(e)
	default:
		err = fmt.Errorf("operación desconocida %q", e.Op)
	}
	return err
}

// --- registrar en el edit log ---

// record registra e en el edit log antes de responder al cliente. Si falla ya
// respondió 500 y devuelve false. Con up.mu tomado. Sin persistencia activada
// no hace nada.
func (s *RESTServer) record(w http.ResponseWriter, e *EditEntry) bool {
	if err := s.journal(e); err != nil {
		log.Printf("ERROR: no se pudo registrar %s %s en el edit log: %v", e.Op, e.Path, err)
		writeError(w, http.StatusInternalServerError, codeInternal,
			"no se pudo registrar el cambio en el edit log")
		return false
	}
	return true
}

func (s *RESTServer) journal(e *EditEntry) error {
	if s.log == nil {
		return nil
	}
	err := s.log.Append(e)
	s.noteTx() // aunque falle la replicacion, la entrada ya esta en el log
	if err != nil {
		return err
	}
	s.maybeCheckpoint()
	return nil
}

// maybeCheckpoint guarda un snapshot si ya se acumularon snapshotEvery entradas.
// Con up.mu tomado.
func (s *RESTServer) maybeCheckpoint() {
	if s.log == nil || s.snapshotEvery <= 0 || s.log.SinceCheckpoint() < s.snapshotEvery {
		return
	}
	if err := s.checkpointLocked(); err != nil {
		// No es fatal: el log sigue teniendo todo; se reintenta en la proxima.
		log.Printf("AVISO: no se pudo guardar el snapshot: %v", err)
	}
}

// --- snapshot ---

// buildSnapshot captura el estado completo. Con up.mu tomado (no hay mutaciones).
func (s *RESTServer) buildSnapshot(lastTxID int64) *snapshotFile {
	files, blocks := s.blocks.export()
	uploads := make(map[string]*snapUpload, len(s.up.byPath))
	for path, u := range s.up.byPath {
		su := &snapUpload{ID: u.id, Owner: u.owner, Attempts: make(map[int]int, len(u.attempts))}
		for _, c := range u.committed {
			su.Committed = append(su.Committed, snapCommitted{ID: c.id, Size: c.size, Checksum: c.checksum})
		}
		if u.pending != nil {
			su.Pending = &snapPending{ID: u.pending.id, Index: u.pending.index, Size: u.pending.size}
		}
		for i, n := range u.attempts {
			su.Attempts[i] = n
		}
		uploads[path] = su
	}
	return &snapshotFile{
		Version:  snapshotVersion,
		LastTxID: lastTxID,
		Time:     s.stamp(),
		Root:     s.ns.export(),
		Files:    files,
		Blocks:   blocks,
		Uploads:  uploads,
	}
}

// restoreSnapshot reemplaza el estado por el del snapshot. Con up.mu tomado.
func (s *RESTServer) restoreSnapshot(snap *snapshotFile) error {
	if err := s.ns.restore(snap.Root); err != nil {
		return err
	}
	s.blocks.restore(snap.Files, snap.Blocks)
	s.up.byPath = make(map[string]*upload, len(snap.Uploads))
	for path, su := range snap.Uploads {
		u := &upload{id: su.ID, owner: su.Owner, attempts: make(map[int]int, len(su.Attempts))}
		for _, c := range su.Committed {
			u.committed = append(u.committed, committedBlock{id: c.ID, size: c.Size, checksum: c.Checksum})
			u.committedSize += c.Size
		}
		if su.Pending != nil {
			u.pending = &pendingBlock{id: su.Pending.ID, index: su.Pending.Index, size: su.Pending.Size}
		}
		for i, n := range su.Attempts {
			u.attempts[i] = n
		}
		s.up.byPath[path] = u
	}
	return nil
}

// Checkpoint guarda un snapshot del estado y vacía el edit log. No hace nada si
// no hay cambios desde el último o si la persistencia no está activada.
func (s *RESTServer) Checkpoint() error {
	s.up.mu.Lock()
	defer s.up.mu.Unlock()
	return s.checkpointLocked()
}

func (s *RESTServer) checkpointLocked() error {
	if s.log == nil || s.log.SinceCheckpoint() == 0 {
		return nil
	}
	// El snapshot se escribe completo y de forma atómica ANTES de vaciar el log:
	// si el proceso cae entre ambos pasos, al reiniciar se ignoran las entradas
	// del log que el snapshot ya incluye (txid <= LastTxID).
	if err := writeSnapshot(s.log.dir, s.buildSnapshot(s.log.LastTxID())); err != nil {
		return err
	}
	return s.log.Reset()
}

// --- recuperación ---

// EnablePersistence activa la persistencia de metadatos en dir y recupera el
// estado guardado: carga el snapshot (si existe) y reproduce el edit log. Debe
// llamarse una sola vez, al arrancar y antes de atender peticiones. Si los
// archivos están corruptos devuelve error: es preferible no arrancar a servir
// metadatos incorrectos.
//
// snapshotEvery es cada cuántas operaciones se guarda un snapshot y se vacía el
// log (0 = solo cuando se llame a Checkpoint).
func (s *RESTServer) EnablePersistence(dir string, snapshotEvery int) error {
	s.up.mu.Lock()
	defer s.up.mu.Unlock()

	if s.log != nil {
		return fmt.Errorf("la persistencia ya está activada")
	}
	l, entries, err := OpenEditLog(dir)
	if err != nil {
		return err
	}
	snap, err := readSnapshot(dir)
	if err != nil {
		_ = l.Close()
		return err
	}

	var last int64
	if snap != nil {
		if err := s.restoreSnapshot(snap); err != nil {
			_ = l.Close()
			return fmt.Errorf("restaurar el snapshot: %w", err)
		}
		last = snap.LastTxID
	}
	replayed := 0
	for _, e := range entries {
		if e.TxID <= last {
			continue // ya incluida en el snapshot
		}
		if e.TxID != last+1 {
			_ = l.Close()
			return fmt.Errorf("el edit log tiene un hueco: se esperaba el txid %d y sigue el %d", last+1, e.TxID)
		}
		if err := s.applyEntry(e); err != nil {
			_ = l.Close()
			return fmt.Errorf("reproducir la entrada %d (%s %s): %w", e.TxID, e.Op, e.Path, err)
		}
		last = e.TxID
		replayed++
	}
	l.resume(last, replayed)
	s.log = l
	s.noteTx()
	s.snapshotEvery = snapshotEvery
	if snap != nil || replayed > 0 {
		log.Printf("metadatos recuperados desde %s: snapshot hasta txid %d + %d entradas del log (último txid %d)",
			dir, snapTxID(snap), replayed, last)
	}
	return nil
}

func snapTxID(snap *snapshotFile) int64 {
	if snap == nil {
		return 0
	}
	return snap.LastTxID
}

// Close cierra el edit log. No guarda un snapshot: para eso llamar antes a Checkpoint.
func (s *RESTServer) Close() error {
	s.up.mu.Lock()
	defer s.up.mu.Unlock()
	if s.log == nil {
		return nil
	}
	err := s.log.Close()
	s.log = nil
	return err
}
