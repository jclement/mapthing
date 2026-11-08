package build

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
)

const webMercatorPRJ = `PROJCS["WGS_1984_Web_Mercator_Auxiliary_Sphere",GEOGCS["GCS_WGS_1984",DATUM["D_WGS_1984",SPHEROID["WGS_1984",6378137.0,298.257223563]],PRIMEM["Greenwich",0.0],UNIT["Degree",0.0174532925199433]],PROJECTION["Mercator_Auxiliary_Sphere"],PARAMETER["False_Easting",0.0],PARAMETER["False_Northing",0.0],PARAMETER["Central_Meridian",0.0],PARAMETER["Standard_Parallel_1",0.0],PARAMETER["Auxiliary_Sphere_Type",0.0],UNIT["Meter",1.0]]`

// writeTinyShapefile writes base.{shp,shx,dbf,prj}: one 1 km square polygon at
// (-19000 km, -19000 km) in EPSG:3857, far outside any region.
func writeTinyShapefile(base string) error {
	x0, y0, x1, y1 := -19000000.0, -19000000.0, -18999000.0, -18999000.0
	ring := [][2]float64{{x0, y0}, {x0, y1}, {x1, y1}, {x1, y0}, {x0, y0}} // clockwise = outer
	le, be := binary.LittleEndian, binary.BigEndian
	f64 := func(b *bytes.Buffer, v float64) { _ = binary.Write(b, le, math.Float64bits(v)) }

	header := func(fileBytes int) []byte {
		var h bytes.Buffer
		_ = binary.Write(&h, be, int32(9994))
		h.Write(make([]byte, 20))
		_ = binary.Write(&h, be, int32(fileBytes/2))
		_ = binary.Write(&h, le, int32(1000))
		_ = binary.Write(&h, le, int32(5)) // polygon
		for _, v := range []float64{x0, y0, x1, y1, 0, 0, 0, 0} {
			f64(&h, v)
		}
		return h.Bytes()
	}

	var content bytes.Buffer
	_ = binary.Write(&content, le, int32(5))
	for _, v := range []float64{x0, y0, x1, y1} {
		f64(&content, v)
	}
	_ = binary.Write(&content, le, int32(1))         // parts
	_ = binary.Write(&content, le, int32(len(ring))) // points
	_ = binary.Write(&content, le, int32(0))         // part 0 starts at point 0
	for _, p := range ring {
		f64(&content, p[0])
		f64(&content, p[1])
	}

	var shp bytes.Buffer
	shp.Write(header(100 + 8 + content.Len()))
	_ = binary.Write(&shp, be, int32(1))
	_ = binary.Write(&shp, be, int32(content.Len()/2))
	shp.Write(content.Bytes())

	var shx bytes.Buffer
	shx.Write(header(100 + 8))
	_ = binary.Write(&shx, be, int32(50))
	_ = binary.Write(&shx, be, int32(content.Len()/2))

	var dbf bytes.Buffer
	dbf.Write([]byte{0x03, 126, 1, 1})
	_ = binary.Write(&dbf, le, uint32(1))       // records
	_ = binary.Write(&dbf, le, uint16(32+32+1)) // header length
	_ = binary.Write(&dbf, le, uint16(1+1))     // record length
	dbf.Write(make([]byte, 20))
	field := make([]byte, 32)
	copy(field, "ID")
	field[11] = 'N'
	field[16] = 1 // length
	dbf.Write(field)
	dbf.WriteByte(0x0D)
	dbf.WriteString(" 1")
	dbf.WriteByte(0x1A)

	for ext, data := range map[string][]byte{
		".shp": shp.Bytes(), ".shx": shx.Bytes(), ".dbf": dbf.Bytes(), ".prj": []byte(webMercatorPRJ),
	} {
		if err := os.WriteFile(base+ext, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
