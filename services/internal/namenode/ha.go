// Alta disponibilidad del NameNode: replicación del edit log al standby (US-71).
//
// El NameNode activo envía cada entrada del edit log al standby y espera su ACK
// antes de responder al cliente. El standby las aplica en orden con el mismo
// código que usa la recuperación (applyEntry) y las guarda en su propio edit
// log, de modo que puede reemplazar al activo sin perder metadatos. Un standby
// responde 503 a toda la API REST hasta que se promueva (US-72).
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
	// ErrNotStandby: se pidió aplicar entradas replicadas a un NameNode activo.
	ErrNotStandby = errors.New("este NameNode no es standby")
	// ErrReplicationGap: las entradas recibidas no continúan el edit log del standby.
	ErrReplicationGap = errors.New("hueco en las entradas del edit log")
	// ErrStandbyNeedsResync: el standby está más atrasado que el historial que el
	// activo conserva en memoria. Hace falta copiarle un snapshot (no implementado).
	ErrStandbyNeedsResync = errors.New("el standby está demasiado atrasado y requiere resincronización")
)

// --- lado standby ---

// SetStandby cambia el rol del NameNode. Un standby responde 503 NAMENODE_STANDBY
// a toda la API REST y acepta entradas con ReplicateEditLog. US-72 lo usa para
// promover al standby.
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
//   - Las entradas con txid ya aplicado se ignoran (reenvíos).
//   - Si una entrada no es la siguiente esperada, se detiene sin aplicarla y
//     devuelve ErrReplicationGap junto con el último txid aplicado: el activo
//     responde reenviando desde el siguiente.
//   - Una lista vacía solo consulta el último txid aplicado.
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
	for _, e := range entries {
		last := s.log.LastTxID()
		if e.TxID <= last {
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

// --- lado activo ---

// StandbyClient envía entradas del edit log al NameNode standby. La implementa el
// cliente gRPC de NameNodeHA.ReplicateEditLog.
type StandbyClient interface {
	// ReplicateEditLog envía entradas consecutivas y devuelve el último txid que
	// el standby tiene aplicado. Si las entradas no continúan su log devuelve ese
	// txid junto con un error que cumpla errors.Is(err, ErrReplicationGap).
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

	mu      sync.Mutex
	history []EditEntry // últimas entradas, en orden
	acked   int64       // último txid que el standby confirmó
}

// AttachStandby activa la replicación al standby: cada entrada nueva del edit log
// se envía con client y se espera su ACK antes de responder al cliente. El
// historial inicial se toma del edit log actual, así que un standby que se quedó
// atrás mientras el activo estaba apagado se pone al día con Sync. Requiere
// persistencia.
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
	entries, err := s.log.Entries()
	if err != nil {
		return nil, err
	}
	r.history = entries
	// Se supone al standby al día; si no lo está, el primer envío lo detecta (hueco).
	r.acked = s.log.LastTxID()
	s.log.SetOnAppend(r.Replicate)
	s.replicator = r
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
				return fmt.Errorf("el standby tiene el txid %d y el activo solo el %d: los logs divergieron", last, top)
			}
			r.acked = last
			if last >= r.topLocked() {
				return nil
			}
		// El standby aplicó menos de lo enviado: se reintenta con lo que falta.
		case errors.Is(err, ErrReplicationGap):
			// El standby pide desde su último txid: se le reenvía desde ahí.
			r.acked = last
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

// pendingLocked devuelve las entradas que el standby aún no ha confirmado.
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
	return append([]EditEntry(nil), pending...), nil
}

// RunStandbySync pone al día al standby cada interval hasta que ctx termine. No
// hace nada si no hay un standby conectado (AttachStandby).
func (s *RESTServer) RunStandbySync(ctx context.Context, interval time.Duration) {
	s.up.mu.Lock()
	r := s.replicator
	s.up.mu.Unlock()
	if r != nil {
		r.RunSync(ctx, interval)
	}
}
