## US-XX: título de la historia

Closes #

### Qué cambia

-

### Cómo probarlo

```
```

### Definición de terminado

- [ ] Todos los criterios de aceptación de la historia se cumplen
- [ ] `go build ./...` y `go vet ./...` pasan en `services/` (si toca Go)
- [ ] `python -m pytest client/tests` pasa (si toca Python)
- [ ] Hay al menos una prueba (unitaria o manual documentada arriba)
- [ ] Sin secretos, IPs fijas ni rutas locales en el código
- [ ] Si cambió un `.proto`: se corrió `scripts/gen_proto` y lo generado va en este PR (etiqueta `contract-change`)
- [ ] Documentación actualizada si cambió un contrato o un comando
- [ ] Solo toca archivos de mi área (o el dueño es revisor)
