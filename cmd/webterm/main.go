// Command webterm levanta el backend de WebTerm: sirve la UI estática,
// persiste las sesiones en SQLite y las expone por HTTP y WebSocket.
//
// Desde M10 son dos procesos en un solo binario: el orquestador (este modo,
// el default) y el daemon (`webterm daemon`), dueño de los ptys. Separarlos
// es lo que permite reiniciar el orquestador sin matar las sesiones vivas.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/giuliano/webterm/internal/control"
	"github.com/giuliano/webterm/internal/daemon"
	"github.com/giuliano/webterm/internal/daemonclient"
	webmcp "github.com/giuliano/webterm/internal/mcp"
	"github.com/giuliano/webterm/internal/resources"
	"github.com/giuliano/webterm/internal/server"
	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

func main() {
	// Un primer argumento que no empieza con "-" es un comando, no un flag, y
	// si no es uno conocido se corta acá en vez de caer al orquestador.
	//
	// Sin este chequeo, `webterm status` —un typo naturalísimo de
	// `webterm daemon status`— arrancaba el orquestador; y como flag.Parse
	// frena en el primer argumento que no es un flag, se comía todos los que
	// vinieran después. El resultado era ignorar el -db y el -addr que pasaste
	// y levantar un daemon contra la base de producción: el castigo más caro
	// posible para un error de tipeo.
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		if os.Args[1] != "daemon" {
			usageErrorf("comando desconocido: %q\nel único comando es `webterm daemon`; sin comando, webterm corre el orquestador",
				os.Args[1])
		}
		os.Args = append(os.Args[:1], os.Args[2:]...)
		runDaemonCommand()
		return
	}
	runOrchestrator()
}

// usageErrorf corta por un error de uso: va a stderr, sin el timestamp de log
// —esto no es un evento del programa, es una invocación mal escrita— y sale
// con 2, que es lo que un script mira para distinguirlo de una operación que
// se intentó y falló.
func usageErrorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "webterm: "+format+"\n", args...)
	os.Exit(2)
}

// runDaemonCommand despacha `webterm daemon` y sus subcomandos.
func runDaemonCommand() {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "base SQLite con el estado de las sesiones")
	historyBytes := fs.Int64("history-bytes", session.DefaultHistoryBytes, "cuánto output se guarda por sesión")
	assumeYes := fs.Bool("yes", false, "no pedir confirmación antes de matar las sesiones vivas (stop y restart)")

	sub, err := parseDaemonArgs(fs, os.Args[1:])
	if err != nil {
		usageErrorf("%v", err)
	}
	paths, err := daemon.PathsFor(*dbPath)
	if err != nil {
		log.Fatal(err)
	}

	switch sub {
	case "": // `webterm daemon`: corre en foreground
		if err := runDaemon(*dbPath, *historyBytes); err != nil {
			log.Fatal(err)
		}
	case "status":
		cl := daemonclient.New(paths.Socket)
		defer cl.Close()
		info, err := cl.Info()
		if err != nil {
			fmt.Printf("no hay daemon corriendo en %s\n", paths.Socket)
			os.Exit(1)
		}
		live, _ := cl.LiveIDs()
		fmt.Printf("daemon pid %d, protocolo %d, %d sesiones vivas\n",
			info.PID, info.ProtocolVersion, len(live))
		if info.ProtocolVersion != daemon.ProtocolVersion {
			fmt.Printf("¡atención! este binario habla protocolo %d: hace falta `webterm daemon restart`\n",
				daemon.ProtocolVersion)
		}
	case "stop":
		if !confirmKill(paths, "stop", *assumeYes) {
			fmt.Println("cancelado; el daemon sigue corriendo")
			return
		}
		if err := stopDaemon(paths); err != nil {
			log.Fatal(err)
		}
		fmt.Println("daemon detenido; las sesiones que tenía vivas murieron con él")
	case "restart":
		if !confirmKill(paths, "restart", *assumeYes) {
			fmt.Println("cancelado; el daemon sigue corriendo")
			return
		}
		if err := stopDaemon(paths); err != nil {
			log.Printf("no había daemon que detener: %v", err)
		}
		cl, err := ensureDaemon(paths, *dbPath, *historyBytes)
		if err != nil {
			log.Fatal(err)
		}
		defer cl.Close()
		fmt.Println("daemon reiniciado")
	case "logs":
		data, err := os.ReadFile(paths.Log)
		if err != nil {
			log.Fatal(err)
		}
		os.Stdout.Write(data)
	default:
		usageErrorf("subcomando desconocido: `webterm daemon %s`\nson: status, stop, restart, logs", sub)
	}
}

// parseDaemonArgs separa el subcomando de los flags de `webterm daemon`,
// acepte el orden que acepte flag.
//
// Se hace en dos tiempos —antes y después de fs.Parse— y no escaneando los
// argumentos a mano porque un escaneo no sabe qué flags llevan valor: en
// `webterm daemon -db status` la palabra "status" es el valor de -db, no un
// subcomando, y el único que puede distinguirlo es el propio FlagSet.
//
// Antes el subcomando se reconocía SOLO si venía pegado a "daemon", así que
// `webterm daemon -db X status` no corría status: caía al caso vacío y
// arrancaba el daemon en foreground, bloqueando la terminal de quien solo
// quería preguntar un pid.
func parseDaemonArgs(fs *flag.FlagSet, args []string) (string, error) {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	// fs.Parse frena en el primer argumento que no es un flag, así que lo que
	// quede solo puede ser el subcomando puesto al final.
	rest := fs.Args()
	if sub == "" && len(rest) > 0 {
		sub, rest = rest[0], rest[1:]
	}
	if len(rest) > 0 {
		return "", fmt.Errorf("argumentos de más en `webterm daemon`: %s", strings.Join(rest, " "))
	}
	return sub, nil
}

// runOrchestrator es el modo default: sirve la UI y la API, delegando los
// ptys al daemon.
func runOrchestrator() {
	var cfg server.Config
	var ctl control.Config
	var dbPath string
	var historyBytes int64
	var noAuth bool
	var printMCP bool

	flag.StringVar(&cfg.Addr, "addr", "127.0.0.1:7788", "dirección de escucha (0.0.0.0:7788 para exponerlo a la red local)")
	flag.StringVar(&cfg.StaticDir, "static", "web/dist", "carpeta con el build del frontend")
	flag.StringVar(&ctl.Shell, "shell", "", "shell a spawnear (default: $SHELL)")
	flag.StringVar(&cfg.Token, "token", "", "token de acceso (default: se genera uno si no escucha solo en loopback)")
	flag.StringVar(&dbPath, "db", defaultDBPath(), "base SQLite con el estado de las sesiones")
	flag.Int64Var(&historyBytes, "history-bytes", session.DefaultHistoryBytes, "cuánto output se guarda por sesión (se reenvía al daemon)")
	flag.BoolVar(&noAuth, "no-auth", false, "no pedir token aunque escuche en la red (peligroso)")
	flag.BoolVar(&printMCP, "mcp-config", false, "imprimir cómo registrar el servidor MCP en Claude Code y salir")
	flag.Parse()

	// El env var y el flag siguen teniendo prioridad; si no hay ninguno, el
	// token sale de disco. Generar uno nuevo por arranque invalidaría el
	// WEBTERM_TOKEN inyectado en los ptys que sobrevivieron al reinicio.
	if cfg.Token == "" {
		cfg.Token = os.Getenv("WEBTERM_TOKEN")
	}
	if noAuth {
		cfg.Token = ""
	}

	// El token se resuelve ANTES de imprimir la config del MCP: si no, la
	// config sale sin el header Authorization y quien la registre se come un
	// 401 en cada request. Que -mcp-config llegue a CREAR el archivo del token
	// es deliberado: lo que se imprime tiene que ser lo que el server va a
	// pedir, y el server lo crearía igual en su primer arranque.
	if cfg.Token == "" && !noAuth && !server.IsLoopback(cfg.Addr) {
		var err error
		cfg.Token, err = server.LoadOrCreateToken(tokenPath(dbPath))
		if err != nil {
			log.Fatal(err)
		}
	}

	if printMCP {
		printMCPConfig(cfg.Addr, cfg.Token)
		return
	}

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("estado en %s", dbPath)

	// El daemon va antes que todo: sin él no se pueden crear sesiones, y el
	// sweep necesita preguntarle qué tiene vivo.
	paths, err := daemon.PathsFor(dbPath)
	if err != nil {
		log.Fatal(err)
	}
	pty, err := ensureDaemon(paths, dbPath, historyBytes)
	if err != nil {
		log.Fatal(err)
	}

	// historyBytes ya no va acá: el historial lo escribe el daemon, y el flag
	// se le reenvía en ensureDaemon.
	ctl.Resources = resources.NewCache(resources.NewRegistry(resources.NewGitHub()))
	// El token viaja al entorno de cada sesión para que el cliente MCP que
	// corra adentro pueda autenticarse. La shell de la sesión ya corre con tus
	// permisos, así que no abre una puerta que no estuviera abierta.
	if cfg.Token != "" {
		ctl.ExtraEnv = append(ctl.ExtraEnv, "WEBTERM_TOKEN="+cfg.Token)
	}
	// El arranque del daemon ya no se le pasa a la Config: el sweep se lo
	// pregunta al dueño de los ptys en cada barrido (ptyapi.Client.StartedAt).
	// Cachearlo acá mentía apenas corrías `webterm daemon restart` sin
	// reiniciar el orquestador, que es justo lo que hace `make daemon-restart`.
	//
	// EnsureDaemon es cómo el sweep se recupera de un daemon caído. Vive acá y
	// no en control porque spawnear el binario propio es capacidad de esta
	// capa: control no sabe de os.Executable ni de flags.
	ctl.EnsureDaemon = func() error {
		cl, err := ensureDaemon(paths, dbPath, historyBytes)
		if err != nil {
			return err
		}
		// El cliente que ya tiene el manager redisca el socket en cada llamada,
		// así que el daemon nuevo le sirve tal cual y este cliente de más solo
		// servía para esperar a que contestara. Se cierra enseguida: dejarlo
		// vivo acumularía un transport por cada caída.
		return cl.Close()
	}
	mgr := control.NewManager(st, pty, ctl)
	cfg.MCP = webmcp.New(mgr).Handler()

	// Start hace el primer sweep contra el daemon. Va antes de escuchar: si
	// no, hay una ventana en la que la API reporta vivas sesiones que el
	// daemon no tiene.
	if err := mgr.Start(); err != nil {
		log.Fatal(err)
	}

	// Al apagar, el orquestador SOLO suelta sus propias conexiones: las
	// sesiones siguen corriendo en el daemon, que es todo el punto de M10.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Print("apagando el orquestador; las sesiones siguen corriendo en el daemon")
		_ = mgr.Close()
		_ = st.Close()
		os.Exit(0)
	}()

	if err := server.New(cfg, mgr).ListenAndServe(); err != nil {
		_ = mgr.Close()
		_ = st.Close()
		log.Fatal(err)
	}
}

// defaultDBPath deja la base en ~/.webterm/webterm.db, fuera del repo.
func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "webterm.db"
	}
	return filepath.Join(home, ".webterm", "webterm.db")
}

// tokenPath deja el token al lado de la base, para que una instancia de
// desarrollo tenga el suyo igual que tiene su propio socket.
func tokenPath(dbPath string) string {
	return strings.TrimSuffix(dbPath, filepath.Ext(dbPath)) + ".token"
}

// printMCPConfig imprime el comando exacto para registrar el servidor MCP.
//
// Las comillas simples importan: sin ellas el shell expandiría las variables
// al registrar el servidor y quedarían congeladas en la config, que es
// exactamente lo contrario de lo que se busca —la gracia es que Claude Code las
// resuelva en cada request, contra la sesión desde la que se lo llama.
func printMCPConfig(addr, token string) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = "", ""
	}
	// Escuchando en todas las interfaces, la dirección útil para el cliente es
	// loopback: Claude corre en la misma máquina que el backend.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "7788"
	}

	fmt.Printf("claude mcp add --transport http webterm http://%s:%s/mcp \\\n", host, port)
	fmt.Printf("  -H 'X-Webterm-Session: ${WEBTERM_SESSION_ID}'")
	if token != "" {
		fmt.Printf(" \\\n  -H 'Authorization: Bearer ${WEBTERM_TOKEN}'")
	}
	fmt.Println()
	fmt.Println()
	fmt.Println("Las variables las expande Claude Code en cada request, así que")
	fmt.Println("con registrarlo una vez alcanza para todas las sesiones.")
}
