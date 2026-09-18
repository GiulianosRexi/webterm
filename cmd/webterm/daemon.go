package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/giuliano/webterm/internal/daemon"
	"github.com/giuliano/webterm/internal/daemonclient"
	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

// daemonReadyTimeout es cuánto esperamos a que el daemon recién spawneado
// conteste en su socket.
const daemonReadyTimeout = 2 * time.Second

// runDaemon es el modo daemon: abre la base, arma el manager de ptys y escucha
// en el socket hasta que lo apaguen.
func runDaemon(dbPath string, historyBytes int64) error {
	paths, err := daemon.PathsFor(dbPath)
	if err != nil {
		return err
	}

	// El flock es lo que impide dos daemons sobre la misma base. Se mantiene
	// tomado durante toda la vida del proceso: el SO lo suelta solo al morir,
	// así que un crash no deja el lock trabado.
	lock, err := acquireLock(paths.Lock)
	if err != nil {
		return err
	}
	defer lock.Close()

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	mgr := session.NewManager(st, session.Config{HistoryBytes: historyBytes})
	srv := daemon.NewServer(mgr)

	// Al apagar, matamos los ptys y esperamos el último flush del historial.
	// Lo que quede marcado activo lo corrige el sweep del orquestador.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Print("daemon: apagando, cerrando las sesiones vivas")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = mgr.Close()
		_ = st.Close()
		_ = os.Remove(paths.Socket)
		os.Exit(0)
	}()

	return srv.Serve(paths.Socket)
}

// ensureDaemon devuelve un cliente contra un daemon vivo, levantándolo si hace
// falta. Es lo que corre el orquestador antes de escuchar.
//
// historyBytes se le reenvía al daemon porque el historial es suyo: el flag
// del orquestador sería letra muerta si no se propagara.
func ensureDaemon(paths daemon.Paths, dbPath string, historyBytes int64) (*daemonclient.Client, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("no se pudo resolver el binario propio: %w", err)
	}
	return ensureDaemonWith(self, paths, dbPath, historyBytes)
}

// ensureDaemonWith es ensureDaemon con el binario explícito, para poder
// probarlo sin depender de os.Executable (que en un test apunta al test).
func ensureDaemonWith(bin string, paths daemon.Paths, dbPath string, historyBytes int64) (*daemonclient.Client, error) {
	cl := daemonclient.New(paths.Socket)
	switch err := cl.Check(); {
	case err == nil:
		return cl, nil
	case errors.Is(err, daemonclient.ErrProtocolMismatch):
		// Recompilaste y el daemon que corre quedó viejo. No arrancamos: es
		// mejor decir qué hacer que fallar raro a mitad de un attach.
		_ = cl.Close()
		return nil, fmt.Errorf("%w\ncorré `webterm daemon restart` (mata las sesiones vivas)", err)
	}
	_ = cl.Close()

	if err := spawnDaemon(bin, dbPath, paths, historyBytes); err != nil {
		return nil, err
	}

	cl = daemonclient.New(paths.Socket)
	deadline := time.Now().Add(daemonReadyTimeout)
	for {
		if err := cl.Check(); err == nil {
			return cl, nil
		} else if errors.Is(err, daemonclient.ErrProtocolMismatch) {
			_ = cl.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = cl.Close()
			return nil, fmt.Errorf("el daemon no respondió en %s; mirá %s", daemonReadyTimeout, paths.Log)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// spawnDaemon lanza el daemon desatado de este proceso.
func spawnDaemon(bin, dbPath string, paths daemon.Paths, historyBytes int64) error {
	logFile, err := os.OpenFile(paths.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("abriendo %s: %w", paths.Log, err)
	}
	defer logFile.Close()

	cmd := exec.Command(bin, "daemon", "-db", dbPath,
		"-history-bytes", strconv.FormatInt(historyBytes, 10))
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Setsid es lo que saca al daemon de nuestro grupo de procesos. Sin esto,
	// el Ctrl-C que le des a la terminal del orquestador le llega también al
	// daemon y mata exactamente lo que M10 existe para salvar.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("levantando el daemon: %w", err)
	}
	pid := cmd.Process.Pid
	// No esperamos al proceso: es un daemon, tiene que sobrevivirnos. Release
	// suelta el handle sin esperar (a diferencia de Wait) y lo deja huérfano,
	// para que lo adopte init/launchd. Se llama en el mismo goroutine y antes
	// de loguear: hacerlo async carreaba contra la lectura de cmd.Process.Pid
	// de más abajo sin ganar nada, porque Release no bloquea.
	if err := cmd.Process.Release(); err != nil {
		log.Printf("liberando el proceso del daemon (pid %d): %v", pid, err)
	}

	log.Printf("daemon levantado (pid %d); log en %s", pid, paths.Log)
	return nil
}

// acquireLock toma un flock exclusivo y no bloqueante.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("abriendo %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("ya hay un daemon para esta base (%s): %w", path, err)
	}
	return f, nil
}

// stopDaemon le manda SIGTERM al daemon de estas rutas.
func stopDaemon(paths daemon.Paths) error {
	cl := daemonclient.New(paths.Socket)
	defer cl.Close()
	info, err := cl.Info()
	if err != nil {
		return fmt.Errorf("no hay daemon corriendo en %s", paths.Socket)
	}
	p, err := os.FindProcess(info.PID)
	if err != nil {
		return err
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	// Esperamos a que el socket deje de aceptar: así `daemon restart` no
	// intenta levantar el nuevo antes de que el viejo suelte el lock.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", paths.Socket); err != nil {
			return nil
		} else {
			_ = c.Close()
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("el daemon (pid %d) no terminó a tiempo", info.PID)
}
