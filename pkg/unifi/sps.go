package unifi

import (
	"github.com/AlexxIT/go2rtc/pkg/bits"
)

// withTiming returns the H.264 SPS with VUI timing info for fps added, or the
// SPS unchanged when it already has a VUI or can't be parsed.
//
// The Reolink doorbell sends an SPS without VUI, so the stream carries no
// frame rate, which Protect's live view needs: num_units_in_tick 1,
// time_scale 2*fps, fixed_frame_rate_flag 0 (night mode lowers the rate).
func withTiming(sps []byte, fps int) []byte {
	if fps <= 0 || len(sps) < 4 {
		return sps
	}

	rbsp := unescapeRBSP(sps[1:])
	pos, ok := vuiFlagPos(rbsp)
	if !ok {
		return sps
	}

	r := bits.NewReader(rbsp)
	w := bits.NewWriter(nil)
	for i := 0; i < pos; i++ {
		w.WriteBit(r.ReadBit())
	}
	if r.ReadBit() != 0 {
		return sps // VUI present: leave it alone
	}

	w.WriteBit(1)                  // vui_parameters_present_flag
	w.WriteBit(0)                  // aspect_ratio_info_present_flag
	w.WriteBit(0)                  // overscan_info_present_flag
	w.WriteBit(0)                  // video_signal_type_present_flag
	w.WriteBit(0)                  // chroma_loc_info_present_flag
	w.WriteBit(1)                  // timing_info_present_flag
	w.WriteBits(1, 32)             // num_units_in_tick
	w.WriteBits(uint32(2*fps), 32) // time_scale
	w.WriteBit(0)                  // fixed_frame_rate_flag
	w.WriteBit(0)                  // nal_hrd_parameters_present_flag
	w.WriteBit(0)                  // vcl_hrd_parameters_present_flag
	w.WriteBit(0)                  // pic_struct_present_flag
	w.WriteBit(0)                  // bitstream_restriction_flag

	w.WriteBit(1) // rbsp_stop_one_bit; the rest of the byte is already zero

	return append([]byte{sps[0]}, escapeRBSP(w.Bytes())...)
}

// vuiFlagPos parses an SPS RBSP (without the NAL header byte) up to
// vui_parameters_present_flag and returns that flag's bit position.
func vuiFlagPos(b []byte) (int, bool) {
	r := bits.NewReader(b)

	profile := r.ReadByte()
	_ = r.ReadByte() // constraint flags
	_ = r.ReadByte() // level_idc
	_ = r.ReadUEGolomb()

	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma := r.ReadUEGolomb()
		if chroma == 3 {
			_ = r.ReadBit() // separate_colour_plane_flag
		}
		_ = r.ReadUEGolomb()  // bit_depth_luma_minus8
		_ = r.ReadUEGolomb()  // bit_depth_chroma_minus8
		_ = r.ReadBit()       // qpprime_y_zero_transform_bypass_flag
		if r.ReadBit() != 0 { // seq_scaling_matrix_present_flag
			n := 8
			if chroma == 3 {
				n = 12
			}
			for i := 0; i < n; i++ {
				if r.ReadBit() == 0 {
					continue
				}
				size := 16
				if i >= 6 {
					size = 64
				}
				last, next := int32(8), int32(8)
				for j := 0; j < size; j++ {
					if next != 0 {
						next = (last + r.ReadSEGolomb() + 256) % 256
					}
					if next != 0 {
						last = next
					}
				}
			}
		}
	}

	_ = r.ReadUEGolomb()      // log2_max_frame_num_minus4
	switch r.ReadUEGolomb() { // pic_order_cnt_type
	case 0:
		_ = r.ReadUEGolomb() // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		_ = r.ReadBit()
		_ = r.ReadSEGolomb()
		_ = r.ReadSEGolomb()
		n := r.ReadUEGolomb()
		if n > 255 {
			return 0, false
		}
		for i := uint32(0); i < n; i++ {
			_ = r.ReadSEGolomb()
		}
	}
	_ = r.ReadUEGolomb()  // max_num_ref_frames
	_ = r.ReadBit()       // gaps_in_frame_num_value_allowed_flag
	_ = r.ReadUEGolomb()  // pic_width_in_mbs_minus1
	_ = r.ReadUEGolomb()  // pic_height_in_map_units_minus1
	if r.ReadBit() == 0 { // frame_mbs_only_flag
		_ = r.ReadBit() // mb_adaptive_frame_field_flag
	}
	_ = r.ReadBit()       // direct_8x8_inference_flag
	if r.ReadBit() != 0 { // frame_cropping_flag
		for i := 0; i < 4; i++ {
			_ = r.ReadUEGolomb()
		}
	}

	if r.EOF {
		return 0, false
	}
	idx, left := r.Pos() // index of the current byte, bits of it not yet read
	return (idx+1)*8 - int(left), true
}

// unescapeRBSP removes emulation prevention bytes (00 00 03 -> 00 00).
func unescapeRBSP(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, c)
	}
	return out
}

// escapeRBSP inserts emulation prevention bytes where 00 00 is followed by 00..03.
func escapeRBSP(b []byte) []byte {
	out := make([]byte, 0, len(b)+4)
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, c)
	}
	return out
}
