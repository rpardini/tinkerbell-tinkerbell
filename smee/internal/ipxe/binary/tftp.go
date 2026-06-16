package binary

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"
	"github.com/pin/tftp/v3"
	"github.com/tinkerbell/tinkerbell/smee/internal/hardware"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// TFTP config settings.
type TFTP struct {
	Log                  logr.Logger
	EnableTFTPSinglePort bool
	Addr                 netip.AddrPort
	Timeout              time.Duration
	Patch                []byte
	BlockSize            int
	Backend              hardware.BackendReader
	AssetDir             string
}

// ListenAndServe will listen and serve iPXE binaries over TFTP.
func (h *TFTP) ListenAndServe(ctx context.Context) error {
	a, err := net.ResolveUDPAddr("udp", h.Addr.String())
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", a)
	if err != nil {
		return err
	}

	h.Log.Info("starting TFTP server",
		"addr", h.Addr.String(), "singlePort", h.EnableTFTPSinglePort,
		"blockSize", h.BlockSize, "timeout", h.Timeout.String())

	ts := tftp.NewServer(h.HandleRead, h.HandleWrite)
	ts.SetTimeout(h.Timeout)
	ts.SetBlockSize(h.BlockSize)
	if h.EnableTFTPSinglePort {
		ts.EnableSinglePort()
	}

	go func() {
		<-ctx.Done()
		if err := conn.Close(); err != nil {
			h.Log.Error(err, "failed to close connection")
		}
		ts.Shutdown()
	}()

	return ts.Serve(conn)
}

// HandleRead handlers TFTP GET requests. The function signature satisfies the tftp.Server.readHandler parameter type.
func (h TFTP) HandleRead(filename string, rf io.ReaderFrom) error {
	client := net.UDPAddr{}
	if rpi, ok := rf.(tftp.OutgoingTransfer); ok {
		client = rpi.RemoteAddr()
	}

	full := filename
	filename = path.Base(filename)
	log := h.Log.WithValues("event", "get", "filename", filename, "uri", full, "client", client)

	// clients can send traceparent over TFTP by appending the traceparent string
	// to the end of the filename they really want
	longfile := filename // hang onto this to report in traces
	ctx, shortfile, err := extractTraceparentFromFilename(context.Background(), filename)
	if err != nil {
		log.Error(err, "failed to extract traceparent from filename")
	}
	if shortfile != filename {
		log = log.WithValues("shortfile", shortfile)
		log.Info("traceparent found in filename", "filenameWithTraceparent", longfile)
		filename = shortfile
	}
	// If a mac address is provided (0a:00:27:00:00:02/snp.efi), parse and log it.
	// Mac address is optional.
	optionalMac, _ := net.ParseMAC(path.Dir(full))
	log = log.WithValues("macFromURI", optionalMac.String())

	tracer := otel.Tracer("TFTP")
	_, span := tracer.Start(ctx, "TFTP get",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attribute.String("filename", filename)),
		trace.WithAttributes(attribute.String("requested-filename", longfile)),
		trace.WithAttributes(attribute.String("ip", client.IP.String())),
		trace.WithAttributes(attribute.String("mac", optionalMac.String())),
	)
	defer span.End()

	req := Request{Filename: full, Base: filepath.Base(shortfile), Client: client}

	emb := EmbeddedIPXERoute{Log: log, Patch: h.Patch}
	if handled, errEmb := emb.TryServe(ctx, req, rf); handled {
		return errEmb
	}

	pxe := PXELinuxMACRoute{Log: log, Resolver: hardware.BackendResolver{Backend: h.Backend}}
	if handled, errPxe := pxe.TryServe(ctx, req, rf); handled {
		return errPxe
	}

	rpi := RPiNetbootRoute{Log: log, Resolver: hardware.BackendResolver{Backend: h.Backend}, AssetDir: h.AssetDir}
	if handled, errRpi := rpi.TryServe(ctx, req, rf); handled {
		return errRpi
	}

	// if AssetDir is set, stream the file directly from disk if found.
	if h.AssetDir != "" {
		servedFromDisk, errDsk := tryServeAssetFromDisk(filename, rf, h, full, log, span)
		if servedFromDisk {
			return errDsk
		}
	}

	// if still not handled, return error; file not found.
	err404 := fmt.Errorf("file [%v] unknown: %w", filepath.Base(shortfile), os.ErrNotExist)
	log.Error(err404, "file unknown")
	span.SetStatus(codes.Error, err404.Error())
	return err404
}

func tryServeAssetFromDisk(filename string, rf io.ReaderFrom, h TFTP, full string, log logr.Logger, span trace.Span) (bool, error) {
	// Join the h.AssetDir with the full requested path ("full") in a secure way; prevent path traversal
	assetPath := filepath.Join(h.AssetDir, full)
	log.Info("attempting to load file from asset dir", "assetPath", assetPath, "assetDir", h.AssetDir)

	file, err := os.Open(assetPath)
	if err != nil {
		log.Error(err, "failed to read file from asset dir", "assetPath", assetPath)
		return false, nil
	}

	defer func() {
		if cerr := file.Close(); cerr != nil {
			log.Error(cerr, "failed to close file", "assetPath", assetPath)
		}
	}()

	log.Info("streaming file directly from asset dir", "assetPath", assetPath)

	bytesSent, err := rf.ReadFrom(file)
	if err != nil {
		log.Error(err, "file serve failed", "bytesSent", bytesSent)
		span.SetStatus(codes.Error, err.Error())
		return true, err
	}

	log.Info("file served from disk", "bytesSent", bytesSent)
	span.SetStatus(codes.Ok, filename)
	return true, nil
}

// HandleWrite handles TFTP PUT requests. It will always return an error. This library does not support PUT.
func (h TFTP) HandleWrite(filename string, wt io.WriterTo) error {
	err := fmt.Errorf("access_violation: %w", os.ErrPermission)
	client := net.UDPAddr{}
	if rpi, ok := wt.(tftp.OutgoingTransfer); ok {
		client = rpi.RemoteAddr()
	}
	h.Log.Error(err, "client", client, "event", "put", "filename", filename)

	return err
}
