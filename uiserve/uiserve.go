// Package uiserve sirve el bundle embebido de la UI de un core con
// REVALIDACIÓN de caché, y existe para que ese comportamiento viva una sola
// vez en la plataforma.
//
// El bundle viaja bajo un nombre FIJO (/ui/core-<key>-app.js es el contrato
// que apunta el catálogo de la Shell), así que no hay cache-busting por
// nombre. Y http.FileServer sobre un embed.FS no emite ningún metadato de
// caché (los archivos embebidos tienen modtime cero: sin Last-Modified, sin
// ETag), lo que deja al edge aplicar su default: Cloudflare cachea .js por
// horas y cada deploy de UI queda invisible hasta expirar el TTL — con el
// agravante de que el hard refresh del navegador no obliga al edge a
// revalidar, así que el síntoma no se cura desde el cliente
// (HiveStrix/strix-clients#21; strix-expenses 5058ded).
//
// La respuesta es revalidación, no "sin caché": `Cache-Control: no-cache`
// (servir de caché solo tras revalidar) más un ETag por hash de contenido,
// de modo que cada request cuesta a lo sumo un 304 sin cuerpo — y en cuanto
// un deploy cambia el bundle, cambia el ETag y lo que se sirve es la UI
// nueva.
//
// Antes de este paquete, siete cores llevaban una copia idéntica de este
// archivo, y la clase de bug que eso invita ya ocurrió: un core tenía el
// fix y otro no, y nadie lo supo hasta que un deploy quedó invisible. El
// contrato está escrito en shell-core-contract §12 («Caché del bundle»);
// esta es la implementación que lo cumple.
package uiserve

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
)

// Handler sirve `bundle` bajo el prefijo /ui/ con los headers de
// revalidación. Los ETags se calculan UNA vez, al construir el handler: el
// bundle es embebido, así que sus hashes no pueden cambiar en la vida del
// proceso.
func Handler(bundle fs.FS) http.Handler {
	etags := make(map[string]string)
	_ = fs.WalkDir(bundle, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(bundle, path)
		if err != nil {
			return err
		}
		etags[path] = fmt.Sprintf(`"%x"`, sha256.Sum256(data))
		return nil
	})

	files := http.StripPrefix("/ui/", http.FileServer(http.FS(bundle)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if etag, ok := etags[strings.TrimPrefix(r.URL.Path, "/ui/")]; ok {
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("ETag", etag)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}
