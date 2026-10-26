// Alta disponibilidad del NameNode: replicación del edit log al standby (US-71).
//
// El NameNode activo envía cada entrada del edit log al standby y espera su ACK
// antes de responder al cliente. El standby las aplica en orden con el mismo
// código que usa la recuperación (applyEntry) y las guarda en su propio edit
// log, de modo que puede reemplazar al activo sin perder metadatos. Un standby
// responde 503 a toda la API REST hasta que se promueva (failover.go, US-72).
//
// Este archivo no depende del transporte: el activo habla con el standby a
// través de la interfaz StandbyClient y el standby expone ReplicateEditLog. El
// servicio gRPC NameNodeHA (proto/namenode.proto, US-03) solo tiene que
// traducir mensajes a EditEntry y llamar a estas funciones.
package namenode

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// codeNamenodeStandby: el NameNode contactado es standby (api-rest.md, 1.4).
const codeNamenodeStandby = "NAMENODE_STANDBY"

var (
	// ErrNotStandby: se pidió aplicar entradas replicadas a un NameNode que no es
	// standby. Si el activo lo recibe de su par, otro NameNode ya fue promovido y
	// el activo se degrada a standby para que nunca haya dos activos.
	ErrNotStandby = errors.New("este NameNode no es standby")
	// ErrReplicationGap: las entradas recibidas no continúan el edit log del standby.
	ErrReplicationGap = errors.New("hueco en las entradas del edit log")
	// ErrStandbyNeedsResync: el standby está más atrasado que el historial que el
	// activo conserva en memoria. Hace falta copiarle un snapshot (no implementado).
	ErrStandbyNeedsResync = errors.New("el standby está demasiado atrasado y requiere resincronización")
	// ErrLogsDiverged: el standby ya tiene, con un txid recibido, una entrada
	// distinta a la que envía el activo. Pasa si un activo caído tenía entradas que
	// el standby nunca recibió y luego se promovió al standby. Requiere
	// resincronización manual (copiar los metadatos del activo actual).
	ErrLogsDiverged = errors.New("los edit logs divergieron")
)

// --- lado standby ---

// SetStandby cambia el rol del NameNode. Un standby responde 503 NAMENODE_STANDBY
// a toda la API REST y acepta entradas con ReplicateEditLog. Para promover al
// standby usar Promote (failover.go), que además prepara la recuperación.
func (s *RESTServer) SetStandby(standby bool) { s.standby.Store(standby) }

// IsStandby indica si el NameNode es standby.
func (s *RESTServer) IsStandby() bool { return s.standby.Load() }

// standbyGuard responde 503 a todas las rutas mientras el NameNode es standby.
func (s *RESTServer) standbyGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.standby.Load() {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusServiceUnavailable, codeNamenodeStandby, "este NameNode es standby")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ReplicateEditLog es la lógica del RPC NameNodeHA.ReplicateEditLog en el
// standby: aplica y guarda las entradas en orden y devuelve el último txid
// aplicado (el ACK).
//
//   - Las entradas con txid ya aplicado se ignoran (reenvíos), pero se comparan
//     con la que se guardó: si difieren devuelve ErrLogsDiverged.
//   - Si una entrada no es la siguiente esperada, se detiene sin aplicarla y
//     devuelve ErrReplicationGap junto con el último txid aplicado: el activo
//     responde reenviando desde el siguiente.
//   - Una lista vacía solo consulta el último txid aplicado.
//   - En un NameNode que no es standby devuelve ErrNotStandby.
//
// Requiere persistencia: el standby debe poder sobrevivir a un reinicio.
func (s *RESTServer) ReplicateEditLog(entries []EditEntry) (int64, error) {
	s.up.mu.Lock()
	defer s.up.mu.Unlock()

	if !s.standby.Load() {
		return 0, ErrNotStandby
	}
	if s.log == nil {
		return 0, fmt.Errorf("el standby requiere persistencia (META_DIR) para guardar las entradas")
	}
	defer s.noteTx()
	for _, e := range entries {
		last := s.log.LastTxID()
		if e.TxID <= last {
			if mine, ok := s.log.EntryAt(e.TxID); ok && !sameEntry(mine, e) {
				return last, fmt.Errorf("%w: el txid %d es %s %s aquí y %s %s en el activo",
					ErrLogsDiverged, e.TxID, mine.Op, mine.Path, e.Op, e.Path)
			}
			continue
		}
		if e.TxID != last+1 {
			return last, fmt.Errorf("%w: se esperaba el txid %d y llegó el %d", ErrReplicationGap, last+1, e.TxID)
		}
		if err := s.applyEntry(e); err != nil {
			return last, fmt.Errorf("aplicar la entrada %d (%s %s): %w", e.TxID, e.Op, e.Path, err)
		}
		if err := s.log.AppendReplicated(e); err != nil {
			return last, err
		}
		s.maybeCheckpoint()
	}
	return s.log.LastTxID(), nil
}

// sameEntry indica si dos entradas son la misma operación.
func sameEntry(a, b EditEntry) bool {
	return a.TxID == b.TxID && a.Op == b.Op && a.Time.UnixMilli() == b.Time.UnixMilli() && a.Path == b.Path &&
		a.Owner == b.Owner && a.Parents == b.Parents && a.UploadID == b.UploadID &&
		a.NewUpload == b.NewUpload && a.BlockID == b.BlockID && a.BlockIndex == b.BlockIndex &&
		a.Size == b.Size && a.Checksum == b.Checksum
}

// noteTx publica el último txid del edit log para PingInfo, que no toma locks.
func (s *RESTServer) noteTx() {
	if s.log != nil {
		s.lastTx.Store(s.log.LastTxID())
	}
}

// --- lado activo ---

// StandbyClient envía entradas del edit log al NameNode standby. La implementa el
// cliente gRPC de NameNodeHA.ReplicateEditLog.
type StandbyClient interface {
	// ReplicateEditLog envía entradas consecutivas y devuelve el último txid que
	// el standby tiene aplicado. Los errores del standby deben conservarse para
	// errors.Is:
	//   - ErrReplicationGap: las entradas no continúan su log (junto con su txid).
	//   - ErrNotStandby: ese NameNode ya es activo.
	//   - ErrLogsDiverged: tiene otra entrada con ese txid.
	ReplicateEditLog(ctx context.Context, entries []EditEntry) (lastApplied int64, err error)
}

// ReplicatorOptions configura la replicación al standby.
type ReplicatorOptions struct {
	// Timeout de cada envío al standby. Por defecto 2 s.
	Timeout time.Duration
	// BestEffort: si es true, un standby que no responde NO impide responder al
	// cliente (se registra un aviso y se le reenvía lo pendiente después). Por
	// defecto es false: el cliente solo recibe confirmación si el standby aplicó
	// la operación, que es lo que garantiza que no se pierdan cambios ya
	// confirmados si el activo cae.
	BestEffort bool
	// Retain es cuántas entradas recientes se conservan en memoria para poner al
	// día a un standby atrasado. Por defecto 10000.
	Retain int
}

// Replicator replica las entradas del edit log al standby.
type Replicator struct {
	client  StandbyClient
	timeout time.Duration
	best    bool
	retain  int

	// onFenced se llama cuando el par responde que él es el activo: este NameNode
	// debe degradarse a standby.
	onFenced func()

	mu      sync.Mutex
	history []EditEntry // últimas entradas, en orden
	acked   int64       // último txid que el standby confirmó
}

// AttachStandby activa la replicación al standby: cada entrada nueva del edit log
// se envía con client y se espera su ACK antes de responder al cliente. El
// historial inicial se toma del edit log actual, así que un standby que se quedó
// atrás mientras el activo estaba apagado se pone al día con Sync. Requiere
// persistencia. Si ya había una replicación activa, la reemplaza.
//
// Si el par contesta que él es el activo (ErrNotStandby), este NameNode se
// degrada a standby: nunca debe haber dos activos.
func (s *RESTServer) AttachStandby(client StandbyClient, opts ReplicatorOptions) (*Replicator, error) {
	s.up.mu.Lock()
	defer s.up.mu.Unlock()

	if s.log == nil {
		return nil, fmt.Errorf("la replicación al standby requiere persistencia (META_DIR)")
	}
	if s.standby.Load() {
		return nil, fmt.Errorf("un NameNode standby no replica a otro")
	}
	r := &Replicator{client: client, timeout: opts.Timeout, best: opts.BestEffort, retain: opts.Retain}
	if r.timeout <= 0 {
		r.timeout = 2 * time.Second
	}
	if r.retain <= 0 {
		r.retain = 10000
	}
	r.onFenced = func() {
		log.Printf("AVISO: el otro NameNode es el activo: %s pasa a standby", s.NodeID)
		s.SetStandby(true)
	}
	entries, err := s.log.Entries()
	if err != nil {
		return nil, err
	}
	r.history = entries
	// Se supone al standby al día; si no lo está, el primer envío lo detecta (hueco).
	r.acked = s.log.LastTxID()
	s.log.SetOnAppend(r.Replicate)
	s.replicator.Store(r)
	return r, nil
}

// Replicate envía al standby la entrada e (y todo lo que aún no tenga). Es la
// función que se engancha a EditLog.OnAppend. Si devuelve error, el cliente no
// recibe confirmación de la operación.
func (r *Replicator) Replicate(e EditEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.history = append(r.history, e)
	if len(r.history) > r.retain {
		r.history = append([]EditEntry(nil), r.history[len(r.history)-r.retain:]...)
	}
	err := r.syncLocked()
	if err != nil && r.best {
		log.Printf("AVISO: el standby no confirmó hasta el txid %d (se reintentará): %v", e.TxID, err)
		return nil
	}
	return err
}

// Sync pone al día al standby: consulta hasta dónde tiene aplicado y le envía lo
// que le falta. Sirve para recuperar a un standby que estuvo caído sin esperar
// a la siguiente operación.
func (r *Replicator) Sync() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.syncLocked()
}

// Acked devuelve el último txid confirmado por el standby.
func (r *Replicator) Acked() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.acked
}

// RunSync llama a Sync cada interval hasta que ctx termine.
func (r *Replicator) RunSync(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.Sync(); err != nil {
				log.Printf("AVISO: no se pudo sincronizar con el standby: %v", err)
			}
		}
	}
}

// RunStandbySync pone al día al standby cada interval hasta que ctx termine,
// con la replicación que esté activa en cada momento (cambia al promover o
// degradar el NameNode). No hace nada mientras este NameNode sea standby.
func (s *RESTServer) RunStandbySync(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r := s.replicator.Load()
			if r == nil || s.standby.Load() {
				continue
			}
			if err := r.Sync(); err != nil {
				log.Printf("AVISO: no se pudo sincronizar con el standby: %v", err)
			}
		}
	}
}

// syncLocked envía al standby las entradas posteriores a acked. Con r.mu tomado.
func (r *Replicator) syncLocked() error {
	const maxRounds = 4
	for round := 0; round < maxRounds; round++ {
		pending, err := r.pendingLocked()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
		last, err := r.client.ReplicateEditLog(ctx, pending)
		cancel()

		switch {
		case err == nil:
			if top := r.topLocked(); last > top {
				return fmt.Errorf("%w: el standby tiene el txid %d y el activo solo el %d", ErrLogsDiverged, last, top)
			}
			r.acked = last
			if last >= r.topLocked() {
				return nil
			}
		// El standby aplicó menos de lo enviado: se reintenta con lo que falta.
		case errors.Is(err, ErrReplicationGap):
			// El standby pide desde su último txid: se le reenvía desde ahí.
			r.acked = last
		case errors.Is(err, ErrNotStandby):
			// El par ya es el activo: este NameNode no puede seguir aceptando cambios.
			if r.onFenced != nil {
				r.onFenced()
			}
			return fmt.Errorf("replicar al standby: %w", err)
		default:
			return fmt.Errorf("replicar al standby: %w", err)
		}
	}
	return fmt.Errorf("el standby no logra ponerse al día (confirmado hasta el txid %d)", r.acked)
}

// topLocked devuelve el txid de la última entrada conocida.
func (r *Replicator) topLocked() int64 {
	if n := len(r.history); n > 0 {
		return r.history[n-1].TxID
	}
	return r.acked
}

// pendingLocked devuelve lo que hay que enviar al standby: las entradas que aun no
// ha confirmado, precedidas por la ultima que si confirmo.
func (r *Replicator) pendingLocked() ([]EditEntry, error) {
	i := 0
	for i < len(r.history) && r.history[i].TxID <= r.acked {
		i++
	}
	pending := r.history[i:]
	if len(pending) > 0 && pending[0].TxID != r.acked+1 {
		return nil, fmt.Errorf("%w: confirmado hasta el txid %d y el historial empieza en el %d",
			ErrStandbyNeedsResync, r.acked, pending[0].TxID)
	}
	// Se antepone la ultima entrada ya confirmada: el standby la compara con la suya
	// y asi detecta que los logs divergieron aunque no haya nada nuevo que enviar.
	var send []EditEntry
	if i > 0 && r.history[i-1].TxID == r.acked {
		send = append(send, r.history[i-1])
	}
	return append(send, pending...), nil
}
