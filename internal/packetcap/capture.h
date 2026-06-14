#ifndef PACKET_CAP_H
#define PACKET_CAP_H

#include <stdint.h>
#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

#define PCAP_IFNAME_LEN    64
#define PCAP_FRAME_MAX     9216
#define PCAP_RING_BLOCKS   128
#define PCAP_RING_FRAMES   32
#define PCAP_RING_BLOCKSZ  (1 << 19)

struct capture_stats {
    uint64_t rx_packets;
    uint64_t rx_bytes;
    uint64_t drop_packets;
    uint64_t drop_bytes;
    uint64_t errors;
    uint64_t hw_captured;
};

struct frame_buf {
    uint64_t rx_time_ns;
    uint32_t length;
    uint8_t  data[PCAP_FRAME_MAX];
};

struct capture_handle;

struct capture_handle* capture_open(const char* ifname,
                                    uint16_t ethertype_filter,
                                    int promisc,
                                    int use_zero_copy);

int capture_next_frame(struct capture_handle* h, struct frame_buf* out);

int capture_read_batch(struct capture_handle* h,
                       struct frame_buf* out_batch,
                       int batch_size);

int capture_get_stats(struct capture_handle* h, struct capture_stats* out_stats);

void capture_close(struct capture_handle* h);

int capture_simulate(struct capture_handle* h,
                     struct frame_buf* out_batch,
                     int batch_size,
                     int sample_count_per_frame,
                     uint32_t base_smpcnt);

#ifdef __cplusplus
}
#endif

#endif
