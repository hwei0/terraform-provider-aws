#!/usr/bin/env python3
"""
Analyze the output.json to check if CRUD function names follow expected patterns.
"""

import json
import re
from collections import defaultdict

def parse_resource_type(resource_type):
    """Parse aws_<service>_<resource> into components."""
    match = re.match(r'^aws_([^_]+)_(.+)$', resource_type)
    if match:
        return match.group(1), match.group(2)
    return None, None

def parse_function_name(func_name):
    """Parse function name to extract service and function."""
    if not func_name:
        return None, None, None

    # Pattern: github.com/hashicorp/terraform-provider-aws/internal/service/<service>.<function>
    match = re.match(r'^github\.com/hashicorp/terraform-provider-aws/internal/service/([^.]+)\.(.+)$', func_name)
    if match:
        return match.group(1), match.group(2), 'service'

    # Check for other patterns (e.g., generated code, flex, etc.)
    match = re.match(r'^github\.com/hashicorp/terraform-provider-aws/internal/([^/]+)/([^.]+)\.(.+)$', func_name)
    if match:
        return match.group(2), match.group(3), match.group(1)

    return None, None, None

def to_camel_case(snake_str):
    """Convert snake_case to CamelCase."""
    components = snake_str.split('_')
    return ''.join(x.title() for x in components)

def main():
    with open('output.json', 'r') as f:
        resources = json.load(f)

    print(f"Analyzing {len(resources)} resources...\n")

    # Statistics
    total_with_create = 0
    pattern_matches = 0
    pattern_mismatches = []
    no_create_function = []

    # Track different patterns
    service_mismatches = defaultdict(list)
    function_name_patterns = defaultdict(int)

    for resource in resources:
        resource_type = resource['resource_type']
        create_func = resource.get('create_without_timeout', '')

        if not create_func:
            no_create_function.append(resource_type)
            continue

        total_with_create += 1

        # Parse resource type
        service_from_type, resource_name = parse_resource_type(resource_type)

        # Parse function name
        service_from_func, func_name, location = parse_function_name(create_func)

        if not service_from_func or not service_from_type or not resource_name:
            pattern_mismatches.append({
                'resource': resource_type,
                'function': create_func,
                'reason': f'Could not parse: service_from_type={service_from_type}, resource_name={resource_name}, service_from_func={service_from_func}'
            })
            continue

        # Expected function name pattern: resource<ResourceName>Create
        expected_func_prefix = f"resource{to_camel_case(resource_name)}Create"

        # Check if service matches
        service_match = service_from_type == service_from_func

        # Check if function name matches expected pattern
        func_match = func_name == expected_func_prefix

        # Track function name patterns
        if func_name.startswith('resource') and func_name.endswith('Create'):
            function_name_patterns['resource<Name>Create'] += 1
        else:
            function_name_patterns[f'Other: {func_name[:30]}...'] += 1

        if service_match and func_match:
            pattern_matches += 1
        else:
            mismatch_info = {
                'resource': resource_type,
                'function': create_func,
                'service_match': service_match,
                'func_match': func_match,
                'expected_service': service_from_type,
                'actual_service': service_from_func,
                'expected_func': expected_func_prefix,
                'actual_func': func_name,
                'location': location
            }
            pattern_mismatches.append(mismatch_info)

            if not service_match:
                service_mismatches[f"{service_from_type} -> {service_from_func}"].append(resource_type)

    # Print results
    print("=" * 80)
    print("SUMMARY")
    print("=" * 80)
    print(f"Total resources: {len(resources)}")
    print(f"Resources with create_without_timeout: {total_with_create}")
    print(f"Resources without create function: {len(no_create_function)}")
    print(f"Pattern matches: {pattern_matches} ({pattern_matches/total_with_create*100:.1f}%)")
    print(f"Pattern mismatches: {len(pattern_mismatches)} ({len(pattern_mismatches)/total_with_create*100:.1f}%)")

    print("\n" + "=" * 80)
    print("FUNCTION NAME PATTERNS")
    print("=" * 80)
    for pattern, count in sorted(function_name_patterns.items(), key=lambda x: -x[1]):
        print(f"{pattern}: {count}")

    if service_mismatches:
        print("\n" + "=" * 80)
        print("SERVICE NAME MISMATCHES (Top 10)")
        print("=" * 80)
        for mismatch, resources in sorted(service_mismatches.items(), key=lambda x: -len(x[1]))[:10]:
            print(f"\n{mismatch}: {len(resources)} resources")
            for r in resources[:3]:
                print(f"  - {r}")
            if len(resources) > 3:
                print(f"  ... and {len(resources) - 3} more")

    if pattern_mismatches:
        print("\n" + "=" * 80)
        print("PATTERN MISMATCHES (First 20)")
        print("=" * 80)
        for i, mismatch in enumerate(pattern_mismatches[:20], 1):
            print(f"\n{i}. {mismatch['resource']}")
            print(f"   Function: {mismatch['function']}")
            if 'reason' in mismatch:
                print(f"   Reason: {mismatch['reason']}")
            else:
                if not mismatch['service_match']:
                    print(f"   Service: expected '{mismatch['expected_service']}', got '{mismatch['actual_service']}'")
                if not mismatch['func_match']:
                    print(f"   Function: expected '{mismatch['expected_func']}', got '{mismatch['actual_func']}'")
                print(f"   Location: {mismatch['location']}")

        if len(pattern_mismatches) > 20:
            print(f"\n... and {len(pattern_mismatches) - 20} more mismatches")

    if no_create_function:
        print("\n" + "=" * 80)
        print(f"RESOURCES WITHOUT CREATE FUNCTION ({len(no_create_function)})")
        print("=" * 80)
        for r in no_create_function[:10]:
            print(f"  - {r}")
        if len(no_create_function) > 10:
            print(f"  ... and {len(no_create_function) - 10} more")

    print("\n" + "=" * 80)
    print("CONCLUSION")
    print("=" * 80)
    if pattern_matches == total_with_create:
        print("✓ All resources follow the expected pattern!")
    else:
        match_rate = pattern_matches / total_with_create * 100
        print(f"Pattern match rate: {match_rate:.1f}%")
        if match_rate > 90:
            print("✓ Most resources follow the expected pattern with some exceptions.")
        elif match_rate > 70:
            print("⚠ Many resources follow the pattern, but there are significant exceptions.")
        else:
            print("✗ The pattern does not hold for most resources.")

if __name__ == '__main__':
    main()