// Failover automático del NameNode (US-72).
//
// El standby hace Ping al activo cada intervalo; tras MaxFailures fallos
// seguidos se promueve (Promote). Un NameNode recién promovido no conoce dónde
// están las réplicas (no se persisten): pide a los DataNodes que se registren de
// nuevo, y mientras no lleguen sus block reports rechaza allocate y locations con
// 503 NAMENODE_RECOVERING. Si el antiguo activo vuelve, se degrada a standby
// (YieldToActivePeer) o, si ya estaba en marcha, al replicarle el nuevo activo
// (ver ErrNotStandby en ha.go): nunca debe haber dos activos.
//
// Como en ha.go, nada aquí depende del transporte: el Ping lo hace una interfaz.
package namenode

import (
	"context"
	"log"
	"net/http"
	"time"
)

// codeNamenodeRecovering: NameNode recién promovido, esperando los block reports.
const codeNamenodeRecovering = "NAMENODE_RECOVERING"

// PingInfo es la respuesta de NameNodeHA.Ping: quién es el NameNode y en qué estado está.
type PingInfo struct {
	NodeID   string
	Active   bool  // true si atiende la API REST
	LastTxID int64 // último txid de su edit log
}

// ActiveProbe consulta al otro NameNode. La implementa el cliente gRPC de
// NameNodeHA.Ping.
type ActiveProbe interface {
	Ping(ctx context.Context) (PingInfo, error)
}

// Reregistrar pide a los DataNodes que se registren de nuevo. Lo implementa el
// registro de DataNodes (US-22): la respuesta del siguiente heartbeat de cada
// DataNode debe llevar registered=false, de modo que se registre y envíe un
// block report completo.
type Reregistrar interface {
	RequestReregistration()
}

// PingInfo es la lógica del RPC NameNodeHA.Ping. No toma locks: debe responder
// aunque este NameNode esté ocupado esperando al standby.
func (s *RESTServer) PingInfo() PingInfo {
	return PingInfo{NodeID: s.NodeID, Active: !s.standby.Load(), LastTxID: s.lastTx.Load()}
}

// Recovering indica si este NameNode, recién promovido, aún espera los block
// reports de los DataNodes.
func (s *RESTServer) Recovering() bool { return s.recovering.Load() }

// rejectIfRecovering responde 503 NAMENODE_RECOVERING si el NameNode aún no
// conoce la ubicación de las réplicas.
func (s *RESTServer) rejectIfRecovering(w http.ResponseWriter) bool {
	if !s.recovering.Load() {
		return false
	}
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusServiceUnavailable, codeNamenodeRecovering,
		"el NameNode fue promovido y espera los block reports de los DataNodes; reintente en un momento")
	return true
}

// PromoteOptions configura la recuperación tras una promoción.
type PromoteOptions struct {
	// ReportThreshold es la fracción de bloques confirmados que deben tener al
	// menos una réplica conocida para terminar la recuperación. Por defecto 0,999.
	ReportThreshold float64
	// ReportWait es el máximo que se espera a los block reports; pasado ese
	// tiempo se termina igualmente (los bloques sin réplicas conocidas quedan
	// como under-replicated). Por defecto 30 s.
	ReportWait time.Duration
	// OnPromoted, si no es nil, se llama una vez promovido (por ejemplo para
	// empezar a replicar hacia el otro NameNode cuando vuelva).
	OnPromoted func()
}

func (o PromoteOptions) withDefaults() PromoteOptions {
	if o.ReportThreshold <= 0 {
		o.ReportThreshold = 0.999
	}
	if o.ReportWait <= 0 {
		o.ReportWait = 30 * time.Second
	}
	return o
}

// Promote convierte este standby en el NameNode activo. Es idempotente: sobre un
// activo no hace nada. Después de promoverlo:
//
//  1. Acepta la API REST (ls, mkdir, rm, put...).
//  2. Pide a los DataNodes que se registren de nuevo (Reregistrar) para recibir
//     sus block reports.
//  3. Rechaza allocate y locations con 503 NAMENODE_RECOVERING hasta que los
//     block reports cubran ReportThreshold de los bloques o venza ReportWait.
func (s *RESTServer) Promote(opts PromoteOptions) {
	opts = opts.withDefaults()

	s.up.mu.Lock()
	if !s.standby.Load() {
		s.up.mu.Unlock()
		return
	}
	s.recovering.Store(true)
	s.standby.Store(false)
	total, _ := s.blocks.ReplicaCoverage()
	s.up.mu.Unlock()

	log.Printf("PROMOCION: %s pasa a NameNode activo (último txid %d, %d bloques por recuperar)",
		s.NodeID, s.lastTx.Load(), total)
	if s.Reregistrar != nil {
		s.Reregistrar.RequestReregistration()
	}
	if opts.OnPromoted != nil {
		opts.OnPromoted()
	}
	go s.waitForBlockReports(opts)
}

// waitForBlockReports termina la recuperación cuando ya se conocen suficientes
// réplicas o se agota el tiempo.
func (s *RESTServer) waitForBlockReports(opts PromoteOptions) {
	deadline := time.Now().Add(opts.ReportWait)
	for {
		total, covered := s.blocks.ReplicaCoverage()
		if total == 0 || float64(covered) >= opts.ReportThreshold*float64(total) {
			s.recovering.Store(false)
			log.Printf("RECUPERACION terminada: %d de %d bloques con réplicas conocidas", covered, total)
			return
		}
		if !time.Now().Before(deadline) {
			s.recovering.Store(false)
			log.Printf("AVISO: la recuperación terminó por tiempo con %d de %d bloques con réplicas conocidas; el resto quedan under-replicated",
				covered, total)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// FailoverOptions configura la detección de caída del activo.
type FailoverOptions struct {
	// Interval entre pings; también es el tiempo máximo de cada ping. Por defecto 1 s.
	Interval time.Duration
	// MaxFailures es cuántos fallos seguidos hacen falta para promover. Por defecto 3.
	MaxFailures int
	// Promote configura la recuperación al promover.
	Promote PromoteOptions
}

func (o FailoverOptions) withDefaults() FailoverOptions {
	if o.Interval <= 0 {
		o.Interval = time.Second
	}
	if o.MaxFailures <= 0 {
		o.MaxFailures = 3
	}
	return o
}

// RunFailover vigila al otro NameNode hasta que ctx termine. Mientras este
// NameNode sea standby hace Ping cada Interval; tras MaxFailures fallos seguidos
// (error de red, timeout, o el par responde que no es activo y este tiene
// prioridad) se promueve. Si este NameNode es activo no hace nada, pero la
// vigilancia continúa por si más adelante se degrada y debe volver a vigilar.
func (s *RESTServer) RunFailover(ctx context.Context, probe ActiveProbe, opts FailoverOptions) {
	opts = opts.withDefaults()
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()

	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !s.standby.Load() {
			failures = 0
			continue
		}

		pctx, cancel := context.WithTimeout(ctx, opts.Interval)
		info, err := probe.Ping(pctx)
		cancel()
		switch {
		case err == nil && info.Active:
			failures = 0
			continue
		case err == nil && !s.winsTieBreak(info):
			// Los dos están en standby y el otro tiene prioridad para promoverse.
			failures = 0
			continue
		case err != nil:
			log.Printf("AVISO: el NameNode activo no responde (%d/%d): %v", failures+1, opts.MaxFailures, err)
		default:
			log.Printf("AVISO: el otro NameNode también es standby y %s tiene prioridad (%d/%d)",
				s.NodeID, failures+1, opts.MaxFailures)
		}
		failures++
		if failures >= opts.MaxFailures {
			s.Promote(opts.Promote)
			failures = 0
		}
	}
}

// winsTieBreak decide, cuando los dos NameNode son standby, cuál se promueve: el
// que tiene más historia (txid mayor) y, a igualdad, el de menor NodeID.
func (s *RESTServer) winsTieBreak(peer PingInfo) bool {
	mine := s.lastTx.Load()
	if mine != peer.LastTxID {
		return mine > peer.LastTxID
	}
	return s.NodeID < peer.NodeID
}

// YieldToActivePeer se usa al arrancar un NameNode configurado como activo: si el
// otro ya es activo (porque tomó el control mientras este estaba caído), este
// arranca como standby para que no haya dos activos. Si el otro no responde o
// es standby, no cambia nada. Devuelve true si se degradó a standby.
func (s *RESTServer) YieldToActivePeer(ctx context.Context, probe ActiveProbe, timeout time.Duration) bool {
	if s.standby.Load() {
		return false
	}
	if timeout <= 0 {
		timeout = time.Second
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	info, err := probe.Ping(pctx)
	if err != nil || !info.Active {
		return false
	}
	if mine := s.lastTx.Load(); mine > info.LastTxID {
		log.Printf("AVISO: %s tiene el txid %d y el activo actual solo el %d: puede haber metadatos divergentes",
			s.NodeID, mine, info.LastTxID)
	}
	log.Printf("%s arranca como standby: %s es el NameNode activo", s.NodeID, info.NodeID)
	s.standby.Store(true)
	return true
}
