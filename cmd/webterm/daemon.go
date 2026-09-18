package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/giuliano/webterm/internal/daemon"
	"github.com/giuliano/webterm/internal/daemonclient"
	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

// daemonReadyTimeout es el techo de cuánto esperamos a que el daemon recién
// spawneado conteste en su socket.
//
// Era de 2 s y no alcanzaba. Un daemon sano contesta en unos 50 ms, pero
// arrancar un proceso, abrir SQLite y correr las migraciones tarda bastante más
// en una máquina cargada: la suite completa con -race pasaba los 2 s y hacía
// fallar el test del arranque on-demand más o menos dos veces de cada tres.
//
// No hay forma barata de acortar el caso malo: el daemon se spawnea con
// Process.Release(), así que si muere al arrancar queda de zombi hasta que
// terminemos nosotros y sigue contestando que existe —verificado—. O sea que
// este reloj es la única señal de "no va a venir", y de ahí que se pague
// esperándolo entero cuando algo salió mal.
const daemonReadyTimeout = 5 * time.Second

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
	//
	// La señal NO termina el proceso desde el goroutine: solo corta el
	// servidor HTTP, para que Serve devuelva y el cierre pase por el camino
	// normal de retorno. Antes se hacía os.Exit(0) acá, y eso dejaba los
	// `defer lock.Close()` y `defer st.Close()` de arriba como decoración:
	// prometían un cierre ordenado que nunca corría.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Print("daemon: apagando, cerrando las sesiones vivas")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	err = srv.Serve(paths.Socket)
	// mgr.Close() va antes de que corran los defers: mata los ptys y espera el
	// último flush del historial, y eso escribe en la base que st.Close() está
	// por cerrar.
	_ = mgr.Close()
	_ = os.Remove(paths.Socket)
	return err
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

	pid, err := spawnDaemon(bin, dbPath, paths, historyBytes)
	if err != nil {
		return nil, err
	}

	cl = daemonclient.New(paths.Socket)
	deadline := time.Now().Add(daemonReadyTimeout)
	for {
		// El "daemon levantado" se anuncia acá y no en spawnDaemon: fork() no
		// es lo mismo que un daemon listo, y un proceso que muere a los 5 ms
		// hacía que el usuario leyera "daemon levantado (pid N)" seguido de
		// "el daemon no respondió". Se afirma después de que contestó.
		err := cl.Check()
		if err == nil {
			log.Printf("daemon levantado (pid %d); log en %s", pid, paths.Log)
			return cl, nil
		}
		if errors.Is(err, daemonclient.ErrProtocolMismatch) {
			_ = cl.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = cl.Close()
			return nil, fmt.Errorf("el daemon (pid %d) no respondió en %s; mirá %s",
				pid, daemonReadyTimeout, paths.Log)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// spawnDaemon lanza el daemon desatado de este proceso y devuelve su pid.
//
// Devuelve el pid en vez de loguearlo: acá lo único que se sabe es que el
// fork salió bien, que no es lo mismo que "hay un daemon andando". Quien
// espera a que conteste es el que puede afirmarlo.
func spawnDaemon(bin, dbPath string, paths daemon.Paths, historyBytes int64) (int, error) {
	logFile, err := os.OpenFile(paths.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("abriendo %s: %w", paths.Log, err)
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
		return 0, fmt.Errorf("levantando el daemon: %w", err)
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
	return pid, nil
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
	// Lo que esperamos es el flock, no el socket.
	//
	// El daemon cierra el listener antes de soltar el flock, así que "el socket
	// dejó de aceptar" llega antes que "el lock está libre" —medido: 0,19 ms
	// ocioso, 1,17 ms bajo carga—. Hoy no muerde porque arrancar un proceso Go
	// tarda unos 10 ms, pero la garantía que necesita `daemon restart` es
	// justamente la del lock: si el nuevo daemon arranca con el viejo todavía
	// teniéndolo, muere con "ya hay un daemon para esta base". Pollear el
	// propio flock y soltarlo al conseguirlo prueba la condición que importa en
	// vez de una que se le parece.
	deadline := time.Now().Add(5 * time.Second)
	for {
		lock, lerr := acquireLock(paths.Lock)
		if lerr == nil {
			// Lo soltamos enseguida: solo queríamos saber que el viejo ya no lo
			// tiene. Cerrar el archivo suelta el flock.
			_ = lock.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("el daemon (pid %d) no soltó %s a tiempo", info.PID, paths.Lock)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// confirmKill avisa cuántas sesiones vivas se va a llevar una operación
// destructiva y, si hay una terminal del otro lado, pide confirmación.
//
// stop y restart son lo único del binario que destruye datos del usuario, y
// hasta acá restart no avisaba nada y stop avisaba después del hecho. El aviso
// sale SIEMPRE; la confirmación solo cuando stdin es una terminal, porque un
// `make daemon-restart` adentro de un script no tiene quién conteste y colgarse
// esperando sería peor que el daño que se intenta evitar. Para ese caso —y para
// el que ya sabe lo que hace— está -yes.
func confirmKill(paths daemon.Paths, action string, assumeYes bool) bool {
	cl := daemonclient.New(paths.Socket)
	defer cl.Close()
	live, err := cl.LiveIDs()
	if err != nil {
		// No hay daemon, o no contesta: no hay nada que destruir, y el propio
		// stop va a explicar mejor qué pasó.
		return true
	}
	if len(live) == 0 {
		return true
	}

	noun := "sesiones vivas"
	if len(live) == 1 {
		noun = "sesión viva"
	}
	fmt.Fprintf(os.Stderr, "¡atención! `webterm daemon %s` va a matar %d %s (%s)\n",
		action, len(live), noun, paths.Socket)

	if assumeYes {
		return true
	}
	if !isInteractive() {
		fmt.Fprintln(os.Stderr, "no hay terminal para confirmar, así que sigo; pasá -yes para no ver este aviso")
		return true
	}
	fmt.Fprint(os.Stderr, "¿seguir? [s/N] ")
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && answer == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "s", "si", "sí", "y", "yes":
		return true
	}
	return false
}

// isInteractive dice si stdin es una terminal, o sea si hay alguien del otro
// lado capaz de contestar una pregunta.
//
// Se pregunta con un ioctl de verdad y no mirando os.ModeCharDevice: /dev/null
// TAMBIÉN es un dispositivo de caracteres, así que un `webterm daemon restart
// < /dev/null` —la forma canónica de correr algo sin entrada— se hacía pasar
// por interactivo, leía EOF y cancelaba la operación que le pediste.
func isInteractive() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd())
}
