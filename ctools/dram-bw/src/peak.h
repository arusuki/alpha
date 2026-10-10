#ifndef DBW_PEAK_H
#define DBW_PEAK_H

struct dbw_peak_estimate {
    double bytes_per_sec;
    unsigned channels;
    unsigned dimms;
    char source[32];
    char detail[256];
};

/* Rome's complete PMU coverage, not CPU cores or NUMA nodes. The path is
 * explicit so firmware decoding can be tested without privileged hardware. */
void dbw_peak_estimate_rome(const char *dmi_path, unsigned max_channels,
                            struct dbw_peak_estimate *out);

#endif
